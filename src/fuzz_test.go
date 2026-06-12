package main

// Fuzz seed for the inspect-JSON IP extractor. In CI this runs as a plain
// seed-corpus test; locally `go test -fuzz=FuzzExtractIPv4 ./src` explores.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var fuzzIPv4RE = regexp.MustCompile(`^(?:\d{1,3}\.){3}\d{1,3}$`)

func FuzzExtractIPv4(f *testing.F) {
	seeds := []string{
		`{"address":"192.168.65.2/24","gateway":"192.168.65.1"}`,
		`{"networks":[{"ipAddress":"10.0.0.7"}]}`,
		`{"dns":["8.8.8.8"],"nameserver":"1.1.1.1"}`,
		`{"a":{"b":[{"ip":"172.16.5.4/16"}]}}`,
		`"10.1.2.3"`,
		`{"address":"127.0.0.1","bind":"0.0.0.0"}`,
		`[]`,
		`null`,
		`{"address":42,"up":true}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		if json.Unmarshal(data, &v) != nil {
			t.Skip("not JSON")
		}
		got := extractIPv4(v) // must never panic
		if got == "" {
			return
		}
		if !fuzzIPv4RE.MatchString(got) {
			t.Errorf("extractIPv4 returned non-IPv4 %q for input %q", got, data)
		}
		if strings.HasPrefix(got, "127.") || got == "0.0.0.0" {
			t.Errorf("extractIPv4 returned excluded address %q for input %q", got, data)
		}
	})
}
