// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package cover

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"sort"
	"strings"

	"cloud.google.com/go/civil"
	"cloud.google.com/go/spanner"
	"github.com/google/syzkaller/pkg/spanner/coveragedb"
	_ "github.com/google/syzkaller/pkg/subsystem/lists"
	"golang.org/x/exp/maps"
	"google.golang.org/api/iterator"
)

type templateHeatmapRow struct {
	Items               []*templateHeatmapRow
	Name                string
	Coverage            []int64
	IsDir               bool
	Depth               int
	LastDayInstrumented int64

	builder      map[string]*templateHeatmapRow
	instrumented map[civil.Date]int64
	covered      map[civil.Date]int64
}

type templateHeatmap struct {
	Root  *templateHeatmapRow
	Dates []string
}

func (thm *templateHeatmapRow) addParts(depth int, pathLeft []string, instrumented, covered int64, dateto civil.Date) {
	thm.instrumented[dateto] += instrumented
	thm.covered[dateto] += covered
	if len(pathLeft) == 0 {
		return
	}
	nextElement := pathLeft[0]
	isDir := len(pathLeft) > 1
	if _, ok := thm.builder[nextElement]; !ok {
		thm.builder[nextElement] = &templateHeatmapRow{
			Name:         nextElement,
			Depth:        depth,
			IsDir:        isDir,
			builder:      make(map[string]*templateHeatmapRow),
			instrumented: make(map[civil.Date]int64),
			covered:      make(map[civil.Date]int64),
		}
	}
	thm.builder[nextElement].addParts(depth+1, pathLeft[1:], instrumented, covered, dateto)
}

func (thm *templateHeatmapRow) prepareDataFor(dates []civil.Date) {
	thm.Items = maps.Values(thm.builder)
	sort.Slice(thm.Items, func(i, j int) bool {
		if thm.Items[i].IsDir != thm.Items[j].IsDir {
			return thm.Items[i].IsDir
		}
		return thm.Items[i].Name < thm.Items[j].Name
	})
	for _, d := range dates {
		var dateCoverage int64
		if thm.instrumented[d] != 0 {
			dateCoverage = 100 * thm.covered[d] / thm.instrumented[d]
		}
		thm.Coverage = append(thm.Coverage, dateCoverage)
	}
	if len(dates) > 0 {
		lastDate := dates[len(dates)-1]
		thm.LastDayInstrumented = thm.instrumented[lastDate]
	}
	for _, item := range thm.builder {
		item.prepareDataFor(dates)
	}
}

type fileCoverageWithDetails struct {
	Filepath     string
	Instrumented int64
	Covered      int64
	Dateto       civil.Date
	Subsystems   []string
}

func filesCoverageToTemplateData(fCov []*fileCoverageWithDetails) *templateHeatmap {
	res := templateHeatmap{
		Root: &templateHeatmapRow{
			builder:      map[string]*templateHeatmapRow{},
			instrumented: map[civil.Date]int64{},
			covered:      map[civil.Date]int64{},
		},
	}
	dates := map[civil.Date]struct{}{}
	for _, fc := range fCov {
		res.Root.addParts(
			0,
			strings.Split(fc.Filepath, "/"),
			fc.Instrumented,
			fc.Covered,
			fc.Dateto)
		dates[fc.Dateto] = struct{}{}
	}
	sortedDates := maps.Keys(dates)
	sort.Slice(sortedDates, func(i, j int) bool {
		return sortedDates[i].Before(sortedDates[j])
	})
	for _, d := range sortedDates {
		res.Dates = append(res.Dates, d.String())
	}

	res.Root.prepareDataFor(sortedDates)
	return &res
}

func filesCoverageWithDetails(ctx context.Context, projectID, ns string, fromDate, toDate civil.Date,
) ([]*fileCoverageWithDetails, error) {
	client, err := coveragedb.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("spanner.NewClient() failed: %s", err.Error())
	}
	defer client.Close()

	stmt := spanner.Statement{
		SQL: `
select
  dateto,
  instrumented,
  covered,
  files.filepath,
  subsystems
from merge_history
  join files
    on merge_history.session = files.session
  join file_subsystems
    on merge_history.namespace = file_subsystems.namespace and files.filepath = file_subsystems.filepath
where
  merge_history.namespace=$1 and dateto>=$2 and dateto<=$3
`,
		Params: map[string]interface{}{
			"p1": ns,
			"p2": fromDate,
			"p3": toDate,
		},
	}

	iter := client.Single().Query(ctx, stmt)
	defer iter.Stop()
	res := []*fileCoverageWithDetails{}
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to iter.Next() spanner DB: %w", err)
		}
		var r fileCoverageWithDetails
		if err = row.ToStruct(&r); err != nil {
			return nil, fmt.Errorf("failed to row.ToStruct() spanner DB: %w", err)
		}
		res = append(res, &r)
	}
	return res, nil
}

func DoHeatMap(w io.Writer, projectID, ns string, dateFrom, dateTo civil.Date) error {
	style, body, js, err := DoHeatMapStyleBodyJS(projectID, ns, dateFrom, dateTo)
	if err != nil {
		return fmt.Errorf("failed to DoHeatMapStyleAndBody() %w", err)
	}
	log.Printf("%s", js)
	return heatmapTemplate.Execute(w, struct {
		Style template.CSS
		Body  template.HTML
		JS    template.HTML
	}{
		Style: style,
		Body:  body,
		JS:    js,
	})
}

func DoHeatMapStyleBodyJS(projectID, ns string, dateFrom, dateTo civil.Date,
) (template.CSS, template.HTML, template.HTML, error) {
	covAndDates, err := filesCoverageWithDetails(context.Background(), projectID, ns, dateFrom, dateTo)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to filesCoverageWithDetails: %w", err)
	}
	templateData := filesCoverageToTemplateData(covAndDates)
	var styles, body, js bytes.Buffer
	if err := heatmapTemplate.ExecuteTemplate(&styles, "style", templateData); err != nil {
		return "", "", "", fmt.Errorf("failed to get styles: %w", err)
	}
	if err := heatmapTemplate.ExecuteTemplate(&body, "body", templateData); err != nil {
		return "", "", "", fmt.Errorf("failed to get body: %w", err)
	}
	if err := heatmapTemplate.ExecuteTemplate(&js, "js", templateData); err != nil {
		return "", "", "", fmt.Errorf("failed to get js: %w", err)
	}
	return template.CSS(styles.String()),
		template.HTML(body.String()),
		template.HTML(js.Bytes()), nil
}

func DoSubsystemsHeatMap(w io.Writer, projectID, ns string, dateFrom, dateTo civil.Date) error {
	covWithDetails, err := filesCoverageWithDetails(context.Background(), projectID, ns, dateFrom, dateTo)
	if err != nil {
		panic(err)
	}
	var ssCovAndDates []*fileCoverageWithDetails
	for _, cwd := range covWithDetails {
		for _, ssName := range cwd.Subsystems {
			newRecord := fileCoverageWithDetails{
				Filepath:     ssName + "/" + cwd.Filepath,
				Instrumented: cwd.Instrumented,
				Covered:      cwd.Covered,
				Dateto:       cwd.Dateto,
			}
			ssCovAndDates = append(ssCovAndDates, &newRecord)
		}
	}
	templateData := filesCoverageToTemplateData(ssCovAndDates)
	return heatmapTemplate.Execute(w, templateData)
}

func approximateInstrumented(points int64) string {
	dim := "_"
	if points > 10000 {
		dim = "K"
		points /= 1000
	}
	return fmt.Sprintf("%d%s", points, dim)
}

//go:embed templates/heatmap.html
var templatesHeatmap string
var templateHeatmapFuncs = template.FuncMap{
	"approxInstr": approximateInstrumented,
}
var heatmapTemplate = template.Must(template.New("").Funcs(templateHeatmapFuncs).Parse(templatesHeatmap))