package domain

import "testing"

func TestNormalizeBranchName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		identifier string
		want       string
	}{
		{
			name:       "keeps ascii branch names",
			raw:        "feature/test-branch",
			identifier: "ABC-1",
			want:       "feature/test-branch",
		},
		{
			name:       "drops non ascii suffixes but keeps ascii prefix",
			raw:        "whdrjs0/jon-67-코드-리뷰해서-리포트-작성하기",
			identifier: "JON-67",
			want:       "whdrjs0/jon-67",
		},
		{
			name:       "falls back to identifier when branch is non ascii only",
			raw:        "코드-리뷰",
			identifier: "JON-67",
			want:       "jon-67",
		},
		{
			name:       "generates fallback when missing",
			raw:        "",
			identifier: "JON-67",
			want:       "jon-67",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeBranchName(tt.raw, tt.identifier); got != tt.want {
				t.Fatalf("NormalizeBranchName(%q, %q) = %q, want %q", tt.raw, tt.identifier, got, tt.want)
			}
		})
	}
}
