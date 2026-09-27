package registry

import (
	"testing"

	"onegit/internal/store"
)

func TestCleanCommand(t *testing.T) {
	for in, want := range map[string]string{
		"COPY app.txt /srv/ # buildkit":            "COPY app.txt /srv/",
		`/bin/sh -c #(nop)  CMD ["sh"]`:            `CMD ["sh"]`,
		"/bin/sh -c apk add --no-cache git":        "RUN apk add --no-cache git",
		"RUN /bin/sh -c go build ./... # buildkit": "RUN /bin/sh -c go build ./...",
	} {
		if got := cleanCommand(in); got != want {
			t.Errorf("cleanCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanupPatterns(t *testing.T) {
	re, err := compilePattern(`pr-\d+`)
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"pr-12": true, "PR-12": true, "pr-12-fix": false, "x-pr-12": false} {
		if got := re.MatchString(v); got != want {
			t.Errorf("match %q = %v, want %v", v, got, want)
		}
	}
	if err := ValidateCleanupRule(&store.RegistryCleanupRule{KeepPattern: "("}); err == nil {
		t.Error("invalid pattern accepted")
	}
	if err := ValidateCleanupRule(&store.RegistryCleanupRule{KeepCount: -1}); err == nil {
		t.Error("negative keep count accepted")
	}
}
