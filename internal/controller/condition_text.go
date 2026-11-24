// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"unicode"
)

const (
	maxConditionMessageRunes = 1024
	maxConditionReasonRunes  = 64
)

// sanitizeConditionMessage makes text that originates outside the control plane,
// such as a provider's error message returned from a CPI probe, safe to persist
// in a status condition and to render in CLIs and UIs. It replaces control
// characters and line breaks with single spaces, drops invisible formatting
// characters (for example bidirectional overrides that can disguise text),
// replaces invalid UTF-8, collapses whitespace, and bounds the length.
func sanitizeConditionMessage(message string) string {
	if message == "" {
		return ""
	}
	message = strings.ToValidUTF8(message, "\uFFFD")

	var out strings.Builder
	runes, pendingSpace := 0, false
	for _, r := range message {
		switch {
		case unicode.IsControl(r), unicode.IsSpace(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			pendingSpace = out.Len() > 0
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		}
		needed := 1
		if pendingSpace {
			needed = 2
		}
		if runes+needed > maxConditionMessageRunes {
			out.WriteString("...")
			break
		}
		if pendingSpace {
			out.WriteByte(' ')
			runes++
			pendingSpace = false
		}
		out.WriteRune(r)
		runes++
	}
	return out.String()
}

// sanitizeConditionReason returns a reason accepted by the CRD condition schema,
// ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$, so that a driver-supplied reason with
// spaces or other characters cannot make the status update fail. Disallowed
// characters are dropped; fallback is used when nothing usable remains. An empty
// reason stays empty.
func sanitizeConditionReason(reason, fallback string) string {
	if reason == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range reason {
		if out.Len() >= maxConditionReasonRunes {
			break
		}
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= '0' && r <= '9', r == '_', r == ',', r == ':':
			if out.Len() > 0 {
				out.WriteRune(r)
			}
		}
	}
	cleaned := strings.TrimRight(out.String(), ",:")
	if cleaned == "" {
		return fallback
	}
	return cleaned
}
