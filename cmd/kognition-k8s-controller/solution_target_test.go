// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"reflect"
	"testing"
)

func TestSolutionTargetExemptUsernamesFromEnv(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		accounts  string
		want      []string
	}{
		{name: "intent gateway", namespace: "kognition-system", accounts: "kognition-intent-gateway", want: []string{"system:serviceaccount:kognition-system:kognition-intent-gateway"}},
		{name: "several with blanks", namespace: "ns", accounts: " a, ,b ", want: []string{"system:serviceaccount:ns:a", "system:serviceaccount:ns:b"}},
		{name: "unset exempts nobody", namespace: "ns", accounts: ""},
		{name: "no namespace exempts nobody", accounts: "a"},
		{name: "a qualified username is not accepted", namespace: "ns", accounts: "system:serviceaccount:other:a", want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", test.namespace)
			t.Setenv("KOGNITION_SOLUTION_TARGET_EXEMPT_SERVICE_ACCOUNTS", test.accounts)
			got := solutionTargetExemptUsernamesFromEnv()
			if len(got) != len(test.want) || (len(got) > 0 && !reflect.DeepEqual(got, test.want)) {
				t.Fatalf("usernames = %v, want %v", got, test.want)
			}
		})
	}
}
