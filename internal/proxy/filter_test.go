package proxy

import "testing"

func TestCompileRule(t *testing.T) {
	cases := []struct {
		name  string
		rule  *FilterRule
		input string
		want  bool
	}{
		{"nil rule allows", nil, "anything", true},
		{"empty list allows", &FilterRule{Mode: FilterModeBlock}, "anything", true},
		{"allow exact hit", &FilterRule{Mode: FilterModeAllow, List: []string{"fetch"}}, "fetch", true},
		{"allow exact miss", &FilterRule{Mode: FilterModeAllow, List: []string{"fetch"}}, "fetch_all", false},
		{"block exact hit", &FilterRule{Mode: FilterModeBlock, List: []string{"rm"}}, "rm", false},
		{"block exact miss", &FilterRule{Mode: FilterModeBlock, List: []string{"rm"}}, "ls", true},
		{"unset mode reads as allow", &FilterRule{List: []string{"fetch"}}, "other", false},
		{"mode is case-insensitive", &FilterRule{Mode: "BLOCK", List: []string{"rm"}}, "rm", false},
		{"prefix glob", &FilterRule{Mode: FilterModeAllow, List: []string{"jira_*"}}, "jira_get", true},
		{"prefix glob miss", &FilterRule{Mode: FilterModeAllow, List: []string{"jira_*"}}, "bitbucket_get", false},
		{"suffix glob", &FilterRule{Mode: FilterModeBlock, List: []string{"*_mutate"}}, "jira_mutate", false},
		{"single-char glob", &FilterRule{Mode: FilterModeAllow, List: []string{"tool?"}}, "tool1", true},
		{"single-char glob is exactly one", &FilterRule{Mode: FilterModeAllow, List: []string{"tool?"}}, "tool12", false},
		{"star is anchored", &FilterRule{Mode: FilterModeAllow, List: []string{"jira_*"}}, "xjira_get", false},
		{"star crosses slash and colon", &FilterRule{Mode: FilterModeBlock, List: []string{"file:///etc/*"}}, "file:///etc/a/b", false},
		{"dot is literal", &FilterRule{Mode: FilterModeAllow, List: []string{"a.b"}}, "axb", false},
		{"any of several patterns", &FilterRule{Mode: FilterModeAllow, List: []string{"a_*", "b_*"}}, "b_x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compileRule("u", "tools", c.rule)(c.input); got != c.want {
				t.Errorf("filter(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}
