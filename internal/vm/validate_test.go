package vm

import (
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// F7 of the 8d-1 review: the API's name check (models.ValidGuestName) is
// this package's rule.
func TestNameRuleIsTheModels(t *testing.T) {
	for _, name := range guestNameCorpus() {
		if nameRE.MatchString(name) != models.ValidGuestName(name) {
			t.Errorf("%q: vm %v, models %v", name, nameRE.MatchString(name), models.ValidGuestName(name))
		}
	}
}

// guestNameCorpus is every single byte, every byte after a valid first
// one, and the lengths around the limit.
func guestNameCorpus() []string {
	out := []string{"", "a b", "web1", "Web.1_x-y", strings.Repeat("a", 62), strings.Repeat("a", 63), strings.Repeat("a", 64), "é", "aé"}
	for b := 0; b < 256; b++ {
		out = append(out, string([]byte{byte(b)}), "a"+string([]byte{byte(b)}))
	}
	return out
}
