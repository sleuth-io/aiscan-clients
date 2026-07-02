package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// prodInstance is the production aiscan instance (matches the extension's
// default).
const prodInstance = "https://app.skills.new"

// defaultInstance is the instance a command targets unless --instance
// overrides it: the AISCAN_INSTANCE environment variable when set (so the
// daemon's LaunchAgent plist can point a machine at a test server without
// flags), otherwise production.
func defaultInstance() string {
	if v := strings.TrimSpace(os.Getenv("AISCAN_INSTANCE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return prodInstance
}

// devicePrompt returns an auth.Prompt that tells the user how to approve the
// device authorization and opens the approval page in their browser.
func devicePrompt(w io.Writer) auth.Prompt {
	return func(userCode, verifyURL string) {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s open %s\n", header("authorize:"), accent(verifyURL))
		if userCode != "" {
			fmt.Fprintf(w, "           and confirm the code %s\n", bold(userCode))
		}
		if err := auth.OpenBrowser(verifyURL); err == nil {
			fmt.Fprintln(w, dim("           (opened your browser…)"))
		}
		fmt.Fprintln(w, dim("waiting for approval…"))
	}
}

// selectRecipes filters recipes to the comma-separated source list, or returns
// them all when the list is empty. An unrecognized source is an error (listing
// the known ones) so a typo'd --source fails loudly instead of silently
// capturing nothing.
func selectRecipes(recipes []capture.Recipe, csv string) ([]capture.Recipe, error) {
	if strings.TrimSpace(csv) == "" {
		return recipes, nil
	}
	want := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	var out []capture.Recipe
	matched := map[string]bool{}
	for _, r := range recipes {
		if want[string(r.ID)] {
			out = append(out, r)
			matched[string(r.ID)] = true
		}
	}
	var unknown []string
	for s := range want {
		if !matched[s] {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown --source %s; known sources: %s",
			strings.Join(unknown, ", "), knownSourceList(recipes))
	}
	return out, nil
}

// knownSourceList is the sorted, comma-separated source ids for help text and
// error messages.
func knownSourceList(recipes []capture.Recipe) string {
	ids := make([]string, 0, len(recipes))
	for _, r := range recipes {
		ids = append(ids, string(r.ID))
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
