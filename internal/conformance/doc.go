// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package conformance provides a portable test suite that validates
// runtime, memory, and tool plugin implementations against the platform
// contracts. Any implementation passing this suite is guaranteed to
// interoperate with the control plane.
//
// Usage in tests:
//
//	func TestNativeRuntime(t *testing.T) {
//	    rt := native.New(...)
//	    conformance.RunRuntimeSuite(t, rt)
//	}
package conformance
