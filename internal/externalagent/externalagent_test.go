// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externalagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestFormatES6NumberVectors(t *testing.T) {
	// RFC 8785 appendix B plus boundary cases of the ECMAScript algorithm.
	cases := map[string]string{
		"0":                      "0",
		"-0":                     "0",
		"5e-324":                 "5e-324",
		"1.7976931348623157e308": "1.7976931348623157e+308",
		"9007199254740992":       "9007199254740992",
		"295147905179352830000":  "295147905179352830000",
		"9.999999999999997e22":   "9.999999999999997e+22",
		"1e23":                   "1e+23",
		"1e21":                   "1e+21",
		"1e20":                   "100000000000000000000",
		"1e-6":                   "0.000001",
		"1e-7":                   "1e-7",
		"333333333.33333329":     "333333333.3333333",
		"4.5":                    "4.5",
		"2e-3":                   "0.002",
		"-1.5E2":                 "-150",
		"12e0":                   "12",
	}
	for input, want := range cases {
		got, err := canonicalNumber(json.Number(input))
		if err != nil || got != want {
			t.Errorf("canonicalNumber(%s) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := canonicalNumber(json.Number("1e999")); err == nil {
		t.Fatal("non-finite number must be rejected")
	}
}

func TestCanonicalCardJSON(t *testing.T) {
	t.Run("sorts members by UTF-16 code unit and escapes minimally", func(t *testing.T) {
		raw := `{"\u20ac":"Euro","\r":"CR","\ufb33":"Hebrew","1":"One","\ud83d\ude00":"Emoji","\u0080":"C1","ö":"o","a":"\u2028\u0007/"}`
		got, err := CanonicalCardJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		want := "{\"\\r\":\"CR\",\"1\":\"One\",\"a\":\"\u2028\\u0007/\",\"\u0080\":\"C1\",\"\u00f6\":\"o\",\"\u20ac\":\"Euro\",\"\U0001F600\":\"Emoji\",\"\ufb33\":\"Hebrew\"}"
		if string(got) != want {
			t.Fatalf("canonical form\n got: %s\nwant: %s", got, want)
		}
	})
	t.Run("removes only top-level signatures", func(t *testing.T) {
		got, err := CanonicalCardJSON([]byte(`{"signatures":[{"protected":"x"}],"name":"a","nested":{"signatures":1}}`))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"name":"a","nested":{"signatures":1}}` {
			t.Fatalf("unexpected canonical form %s", got)
		}
	})
	t.Run("rejects malformed documents", func(t *testing.T) {
		for name, raw := range map[string]string{
			"duplicate member": `{"a":1,"a":2}`,
			"lone high":        `{"a":"\ud83d"}`,
			"lone low":         `{"a":"\ude00"}`,
			"trailing data":    `{"a":1} {}`,
			"not an object":    `[1]`,
			"invalid utf8":     "{\"a\":\"\xff\"}",
			"too deep":         strings.Repeat(`{"a":`, 70) + "1" + strings.Repeat("}", 70),
			"empty":            ``,
		} {
			if _, err := CanonicalCardJSON([]byte(raw)); err == nil {
				t.Errorf("%s: expected rejection", name)
			}
		}
	})
}

func TestCardDigestIsStableAcrossFormatting(t *testing.T) {
	compact := `{"name":"a","skills":[{"id":"x","name":"X"}],"version":"1"}`
	pretty := "{\n  \"version\": \"1\",\n  \"skills\": [ {\"name\": \"X\", \"id\": \"x\"} ],\n  \"name\": \"a\",\n  \"signatures\": [{\"signature\": \"s\"}]\n}"
	first, err := CardDigest([]byte(compact))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CardDigest([]byte(pretty))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("digests differ or are malformed: %s %s", first, second)
	}
	changed, err := CardDigest([]byte(`{"name":"b","skills":[{"id":"x","name":"X"}],"version":"1"}`))
	if err != nil || changed == first {
		t.Fatalf("a changed card must change the digest: %s %v", changed, err)
	}
}

func TestClassifyAddress(t *testing.T) {
	forbidden := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "100.100.100.200", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "2001:db8::1", "2001:0:1::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "2002:a00:1::1", "2002:7f00:1::1",
	}
	for _, text := range forbidden {
		if err := ClassifyAddress(netip.MustParseAddr(text)); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("%s must be forbidden, got %v", text, err)
		}
	}
	permitted := []string{"8.8.8.8", "93.184.216.34", "172.32.0.1", "100.128.0.1", "2606:4700:4700::1111", "64:ff9b::808:808", "2002:808:808::1"}
	for _, text := range permitted {
		if err := ClassifyAddress(netip.MustParseAddr(text)); err != nil {
			t.Errorf("%s must be permitted, got %v", text, err)
		}
	}
	if err := ClassifyAddress(netip.Addr{}); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatal("invalid address must be forbidden")
	}
	if err := ClassifyAddress(netip.MustParseAddr("fe80::1%eth0")); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatal("zoned link-local address must be forbidden")
	}
}

type stubResolver struct {
	addrs []netip.Addr
	err   error
}

func (s stubResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return s.addrs, s.err
}

func TestResolvePermitted(t *testing.T) {
	ctx := context.Background()
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.0.0.7")

	addrs, err := ResolvePermitted(ctx, stubResolver{addrs: []netip.Addr{public}}, "agent.example.com")
	if err != nil || len(addrs) != 1 {
		t.Fatalf("public answer must pass: %v %v", addrs, err)
	}
	if _, err := ResolvePermitted(ctx, stubResolver{addrs: []netip.Addr{public, private}}, "agent.example.com"); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("a mixed answer must be rejected, got %v", err)
	}
	if _, err := ResolvePermitted(ctx, stubResolver{err: errors.New("nxdomain")}, "agent.example.com"); !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("lookup failure must be ErrUnresolvable, got %v", err)
	}
	if _, err := ResolvePermitted(ctx, stubResolver{}, "agent.example.com"); !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("empty answer must be ErrUnresolvable, got %v", err)
	}
}

func TestAllowList(t *testing.T) {
	list, err := ParseAllowList("Agents.Partner.example:443, *.trusted.example:8443\nsolo.example:9000")
	if err != nil {
		t.Fatal(err)
	}
	permitted := [][2]any{
		{"agents.partner.example", 443}, {"a.trusted.example", 8443}, {"a.b.trusted.example", 8443}, {"solo.example", 9000},
	}
	for _, c := range permitted {
		if !list.Permits(c[0].(string), c[1].(int)) {
			t.Errorf("%v must be permitted", c)
		}
	}
	denied := [][2]any{
		{"agents.partner.example", 444}, {"trusted.example", 8443}, {"a.trusted.example", 443},
		{"evil-trusted.example", 8443}, {"other.example", 443}, {"x.agents.partner.example", 443},
	}
	for _, c := range denied {
		if list.Permits(c[0].(string), c[1].(int)) {
			t.Errorf("%v must be denied", c)
		}
	}
	if empty, _ := ParseAllowList("  "); !empty.Empty() || empty.Permits("a.example", 443) {
		t.Fatal("an empty list must permit nothing")
	}
	var nilList *AllowList
	if nilList.Permits("a.example", 443) || !nilList.Empty() {
		t.Fatal("a nil list must permit nothing")
	}
	for _, bad := range []string{"https://a.example:443", "a.example", "10.0.0.1:443", "*.com:443", "*:443", "a.example:0", "*.example"} {
		if _, err := ParseAllowList(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
