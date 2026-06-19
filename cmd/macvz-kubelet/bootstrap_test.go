package main

import (
	"flag"
	"testing"
)

func TestServingTLSSelected(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		genTLS bool
		want   bool
	}{
		{name: "none", want: false},
		{name: "gen tls", genTLS: true, want: true},
		{name: "serving cert", args: []string{"--serving-cert", "/tmp/kubelet.crt"}, want: true},
		{name: "serving key", args: []string{"--serving-key", "/tmp/kubelet.key"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("serving-cert", "/etc/macvz/pki/kubelet.crt", "")
			fs.String("serving-key", "/etc/macvz/pki/kubelet.key", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := servingTLSSelected(fs, tc.genTLS); got != tc.want {
				t.Fatalf("servingTLSSelected = %t, want %t", got, tc.want)
			}
		})
	}
}
