// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externalagent

import (
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// AllowList is the operator egress allow-list. An empty list permits nothing:
// the platform never sends a task to a host the operator did not name.
type AllowList struct {
	exact    map[string]bool
	suffixes map[string]map[int]bool
}

// ParseAllowList parses entries separated by commas, semicolons or whitespace.
// Each entry is "host:port" or "*.domain:port"; a wildcard matches any number
// of leading labels but never the bare domain.
func ParseAllowList(text string) (*AllowList, error) {
	list := &AllowList{exact: map[string]bool{}, suffixes: map[string]map[int]bool{}}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	for _, field := range fields {
		entry := strings.ToLower(field)
		if strings.HasPrefix(entry, "*.") {
			suffix, portValue, err := splitWildcard(entry)
			if err != nil {
				return nil, fmt.Errorf("external agent allow-list entry %q: %w", field, err)
			}
			if list.suffixes[suffix] == nil {
				list.suffixes[suffix] = map[int]bool{}
			}
			list.suffixes[suffix][portValue] = true
			continue
		}
		host, port, err := v1alpha1.ParseExternalAgentEndpoint(entry)
		if err != nil {
			return nil, fmt.Errorf("external agent allow-list entry %q: %w", field, err)
		}
		list.exact[fmt.Sprintf("%s:%d", host, port)] = true
	}
	return list, nil
}

func splitWildcard(entry string) (string, int, error) {
	host, port, err := v1alpha1.ParseExternalAgentEndpoint("x." + strings.TrimPrefix(entry, "*."))
	if err != nil {
		return "", 0, err
	}
	suffix := strings.TrimPrefix(host, "x.")
	if !strings.Contains(suffix, ".") {
		return "", 0, fmt.Errorf("wildcard must cover at least a registered domain, not a top-level domain")
	}
	return suffix, port, nil
}

// Permits reports whether host:port is on the list. host must already be lower
// case (ParseExternalAgentEndpoint guarantees it).
func (l *AllowList) Permits(host string, port int) bool {
	if l == nil {
		return false
	}
	if l.exact[fmt.Sprintf("%s:%d", host, port)] {
		return true
	}
	for suffix, ports := range l.suffixes {
		if ports[port] && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Empty reports whether the list permits nothing.
func (l *AllowList) Empty() bool {
	return l == nil || (len(l.exact) == 0 && len(l.suffixes) == 0)
}
