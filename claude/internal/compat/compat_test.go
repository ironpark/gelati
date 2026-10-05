package compat

import "testing"

func TestEnvTruthy(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"1": true, "true": true, " YES ": true, "on": true, "0": false, "": false, "off": false, "2": false} {
		if got := EnvTruthy(v); got != want {
			t.Errorf("EnvTruthy(%q) = %v", v, got)
		}
	}
}
