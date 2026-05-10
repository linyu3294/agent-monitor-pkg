package scanner

import "testing"

func TestEncodeForCursor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"fix__fe-1098-distribution_amounts_is_None_but_is_distributed-is-true",
			"fix-fe-1098-distribution-amounts-is-None-but-is-distributed-is-true"},
		{"feat__fe-1243-personalize-poker_av_enabled-in-features_status_config",
			"feat-fe-1243-personalize-poker-av-enabled-in-features-status-config"},
		{"chore__cover-poker-v2-tests-100%",
			"chore-cover-poker-v2-tests-100"},
		{"feat__fe-1228-av-enabled-param-on-room-creation-with=eligibility_validation",
			"feat-fe-1228-av-enabled-param-on-room-creation-with-eligibility-validation"},
		{"a/b/c", "a-b-c"},
		{"already-fine", "already-fine"},
		{"trailing--", "trailing"},
		{"", ""},
	}
	for _, c := range cases {
		if got := EncodeForCursor(c.in); got != c.want {
			t.Errorf("EncodeForCursor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
