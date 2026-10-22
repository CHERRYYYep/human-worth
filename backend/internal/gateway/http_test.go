package gateway

import (
	"bytes"
	"crypto/tls"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestContentRange(t *testing.T) {
	for _, test := range []struct {
		name       string
		rangeValue string
		body       int64
		wantOffset int64
		wantTotal  int64
		wantErr    bool
	}{
		{"valid first chunk", "bytes 0-2/3", 3, 0, 3, false},
		{"valid max chunk", "bytes 0-5242879/33554432", 5242880, 0, 33554432, false},
		{"valid last short chunk", "bytes 5242880-5242882/5242883", 3, 5242880, 5242883, false},
		{"old small nonfinal chunk", "bytes 0-262143/33554432", 262144, 0, 0, true},
		{"unaligned offset", "bytes 1-3/4", 3, 0, 0, true},
		{"duplicate header", "bytes 0-2/3", 3, 0, 0, true},
		{"wrong body length", "bytes 0-2/3", 2, 0, 0, true},
		{"chunk too large", "bytes 0-5242880/33554432", 5242881, 0, 0, true},
		{"total too large", "bytes 0-2/33554433", 3, 0, 0, true},
		{"invalid range", "bytes 3-2/3", 0, 0, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("PUT", "https://worth.example.test/", bytes.NewReader(bytes.Repeat([]byte{'x'}, int(test.body))))
			r.Header.Add("Content-Range", test.rangeValue)
			if test.name == "duplicate header" {
				r.Header.Add("Content-Range", test.rangeValue)
			}
			offset, _, total, err := contentRange(r)
			if (err != nil) != test.wantErr || offset != test.wantOffset || total != test.wantTotal {
				t.Fatalf("offset=%d total=%d err=%v", offset, total, err)
			}
		})
	}
}

func TestForwardedHeadersRequireTrustedPeerAndFixedHost(t *testing.T) {
	h, err := New(nil, Options{Origin: "https://worth.example.test", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.8/32")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, host, proto string
		tls                     bool
		want                    int
	}{
		{"direct TLS", "203.0.113.1:50000", "worth.example.test", "", true, 200},
		{"trusted HTTPS proxy", "10.0.0.8:50000", "worth.example.test", "https", false, 200},
		{"forged forwarded proto", "203.0.113.1:50000", "worth.example.test", "https", false, 400},
		{"cleartext", "10.0.0.8:50000", "worth.example.test", "http", false, 400},
		{"forged host", "10.0.0.8:50000", "evil.test", "https", false, 400},
		{"duplicate proto", "10.0.0.8:50000", "worth.example.test", "https, http", false, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://worth.example.test/", nil)
			r.RemoteAddr = test.peer
			r.Host = test.host
			r.Header.Set("X-Forwarded-Proto", test.proto)
			r.Header.Set("X-Forwarded-Host", "worth.example.test")
			if test.tls {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d", w.Code, test.want)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("unsafe response cache/referrer policy")
			}
		})
	}
}
