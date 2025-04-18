// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

struct file_operations {
	void (*open)(void);
	void (*read)(void);
	void (*write)(void);
	void (*read_iter)(void);
	void (*write_iter)(void);
	void (*unlocked_ioctl)(void*, unsigned int, unsigned long);
	void (*mmap)(void);
};

static int alloc_fd() {
	return 1;
}

static void __fget_light(int fd) {
}

static int from_kuid() {
	return 1;
}
