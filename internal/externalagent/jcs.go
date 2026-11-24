// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package externalagent holds the trust-boundary primitives for ExternalAgent:
// canonical Agent Card digests, egress address classification and the operator
// egress allow-list. It has no Kubernetes or network client dependencies so
// that admission, the status reconciler and the transport share one policy.
package externalagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxJSONDepth = 64

// CardDigest returns "sha256:" plus the lowercase hex SHA-256 of the RFC 8785
// canonical form of the Agent Card JSON with its top-level "signatures" member
// removed. The digest is computed over the JSON the endpoint served, so a
// reviewer can reproduce it from the document they inspected.
func CardDigest(raw []byte) (string, error) {
	canonical, err := CanonicalCardJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CanonicalCardJSON returns the RFC 8785 canonical form of an Agent Card with
// the top-level "signatures" member removed. It rejects invalid UTF-8, lone
// surrogate escapes, duplicate object members, trailing data, non-finite
// numbers and documents nested deeper than 64 levels.
func CanonicalCardJSON(raw []byte) ([]byte, error) {
	value, err := parseStrictJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("agent card must be a JSON object")
	}
	delete(object, "signatures")
	var out bytes.Buffer
	if err := writeCanonical(&out, object); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func parseStrictJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("agent card is not valid UTF-8")
	}
	if err := rejectLoneSurrogates(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("agent card has trailing data")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, errors.New("agent card is nested too deeply")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode agent card: %w", err)
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("decode agent card: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("agent card object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("agent card has duplicate member %q", key)
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("decode agent card: %w", err)
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("decode agent card: %w", err)
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// rejectLoneSurrogates fails on a \uD800-\uDFFF escape that is not part of a
// valid high+low pair. encoding/json would silently replace it with U+FFFD,
// which would let two different documents canonicalize identically.
func rejectLoneSurrogates(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) {
			return nil
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		unit, ok := hexUnit(raw, i+2)
		if !ok {
			return nil // malformed escape; the decoder reports it
		}
		i += 5
		switch {
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return errors.New("agent card contains a lone low surrogate escape")
		case unit >= 0xD800 && unit <= 0xDBFF:
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return errors.New("agent card contains a lone high surrogate escape")
			}
			low, ok := hexUnit(raw, i+3)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return errors.New("agent card contains a lone high surrogate escape")
			}
			i += 6
		}
	}
	return nil
}

func hexUnit(raw []byte, offset int) (uint16, bool) {
	if offset+4 > len(raw) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(raw[offset:offset+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(value), true
}

func writeCanonical(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case json.Number:
		formatted, err := canonicalNumber(typed)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case string:
		writeCanonicalString(out, typed)
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return compareUTF16(keys[i], keys[j]) < 0 })
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeCanonicalString(out, key)
			out.WriteByte(':')
			if err := writeCanonical(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

// compareUTF16 orders strings by UTF-16 code units as RFC 8785 section 3.2.3
// requires; this differs from byte order for supplementary-plane characters.
func compareUTF16(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			if ua[i] < ub[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ua) < len(ub):
		return -1
	case len(ua) > len(ub):
		return 1
	}
	return 0
}

func writeCanonicalString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

// canonicalNumber formats a JSON number as the ECMAScript Number-to-String
// algorithm requires (RFC 8785 section 3.2.2.3).
func canonicalNumber(n json.Number) (string, error) {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fmt.Errorf("agent card number %q is not representable", string(n))
	}
	return formatES6Number(f), nil
}

func formatES6Number(f float64) string {
	if f == 0 {
		return "0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64) // d[.ddd]e±XX
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exponent + 1 // position of the decimal point relative to digits
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	expSign := "+"
	e := n - 1
	if e < 0 {
		expSign = "-"
		e = -e
	}
	if k == 1 {
		return sign + digits + "e" + expSign + strconv.Itoa(e)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + expSign + strconv.Itoa(e)
}
