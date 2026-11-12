// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration && windows

package traceability

import (
	"errors"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func stopControllerEnvtest(environment *envtest.Environment) error {
	stopError := environment.Stop()
	if stopError == nil || !strings.Contains(stopError.Error(), "not supported by windows") {
		return stopError
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if entry.ParentProcessID != uint32(os.Getpid()) || (name != "kube-apiserver" && name != "kube-apiserver.exe" && name != "etcd" && name != "etcd.exe") {
			continue
		}
		process, findError := os.FindProcess(int(entry.ProcessID))
		if findError != nil {
			return findError
		}
		killError := process.Kill()
		_ = process.Release()
		if killError != nil && !errors.Is(killError, os.ErrProcessDone) {
			return killError
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	return nil
}
