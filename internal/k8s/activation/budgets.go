// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import "time"

func boundedNamespaceBudget(namespaces int, base, maximum time.Duration) time.Duration {
	if namespaces <= 1 {
		return base
	}
	extra := maximum - base
	if namespaces-1 >= int(extra/time.Minute) {
		return maximum
	}
	return base + time.Duration(namespaces-1)*time.Minute
}

// TransactionTimeout bounds the complete activation for the reviewed inventory.
func TransactionTimeout(namespaces int) time.Duration {
	return boundedNamespaceBudget(namespaces, 5*time.Minute, 20*time.Minute)
}

func (candidate Candidate) TransactionTimeout() time.Duration {
	return TransactionTimeout(len(candidate.manifest.Namespaces))
}

func policyBehaviorTimeout(namespaces int) time.Duration {
	return boundedNamespaceBudget(namespaces, 2*time.Minute, 15*time.Minute)
}
