package proxy

import (
	"log"
	"regexp"
	"strings"
)

// filterFn reports whether one capability — a tool or prompt name, a resource
// URI or URI template — should be re-exposed to downstream clients.
type filterFn func(string) bool

// allowAll is the filter used when a kind has no rule configured.
func allowAll(string) bool { return true }

// filter returns o's capability filter, tolerating a nil receiver (upstreams
// built directly in tests skip load()'s option defaulting).
func (o *OptionsV2) filter() *FilterConfig {
	if o == nil {
		return nil
	}
	return o.Filter
}

// compileRule turns a FilterRule into a predicate. An absent rule or an empty
// list allows everything, so a filter block can be added one kind at a time
// without silently hiding the kinds it doesn't mention. An unset mode reads as
// "allow", matching the pre-filter toolFilter behaviour.
func compileRule(name, kind string, r *FilterRule) filterFn {
	if r == nil || len(r.List) == 0 {
		return allowAll
	}
	pats := make([]*regexp.Regexp, 0, len(r.List))
	for _, p := range r.List {
		re, err := compileGlob(p)
		if err != nil {
			log.Printf("<%s> ignoring invalid %s filter pattern %q: %v", name, kind, p, err)
			continue
		}
		pats = append(pats, re)
	}
	if len(pats) == 0 {
		// Defensive: QuoteMeta makes every pattern compile, so reaching here
		// takes something pathological (a pattern past regexp's size limit).
		// Fail open — an allow rule whose patterns all died would otherwise hide
		// the whole upstream, which is never what a broken pattern meant.
		log.Printf("<%s> %s filter has no usable patterns; exposing all", name, kind)
		return allowAll
	}
	block := FilterMode(strings.ToLower(string(r.Mode))) == FilterModeBlock
	return func(s string) bool {
		for _, re := range pats {
			if re.MatchString(s) {
				return !block
			}
		}
		return block
	}
}

// compileGlob compiles a glob into an anchored regexp: "*" matches any run of
// characters, "?" exactly one, everything else is literal.
//
// Deliberately not path.Match: there "*" stops at "/", and resource URIs are
// full of both "/" and ":", so "file:///etc/*" has to mean what it looks like.
func compileGlob(pat string) (*regexp.Regexp, error) {
	q := regexp.QuoteMeta(pat)
	q = strings.ReplaceAll(q, `\*`, `.*`)
	q = strings.ReplaceAll(q, `\?`, `.`)
	return regexp.Compile("^" + q + "$")
}
