package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vmdisk"
)

// imageImport lays a rootfs file down as a read-only base image LV — the thin
// origin VM disks snapshot from. `boat image-import <name> <file> <disk-gb>`.
func imageImport(arguments []string, errorOutput io.Writer) int {
	if len(arguments) != 3 {
		return usage(errorOutput)
	}
	gigabytes, err := strconv.Atoi(arguments[2])
	if err != nil {
		return reportError(errorOutput, fmt.Errorf("disk-gb: %w", err))
	}
	runner := run.NewRunner(errorOutput)
	if err := vmdisk.ImportBaseImage(context.Background(), runner, arguments[0], arguments[1], gigabytes); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// vmCreateDisk creates a VM's root disk as a thin CoW snapshot of a base image.
// `boat vm-create-disk <uuid> <image-name>`.
func vmCreateDisk(arguments []string, errorOutput io.Writer) int {
	if len(arguments) != 2 {
		return usage(errorOutput)
	}
	runner := run.NewRunner(errorOutput)
	if err := vmdisk.CreateVMDisk(context.Background(), runner, arguments[0], arguments[1]); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}
