package serve

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRecipesTakePlainHTTPWhenTold: an API's address over plain http, to
// a host other than this machine, is refused, as a token sent there is
// read by whatever is on the way; with PLAIN_HTTP=true it is taken, for a
// self-managed tool reached inside a cluster, the firing, incident and
// tracker recipes alike, and curl is given http alone. A user before @ is
// refused still, and PLAIN_HTTP is true or false.
func TestRecipesTakePlainHTTPWhenTold(t *testing.T) {
	needTools(t)
	path, args := recordingCurl(t)
	const plain = "http://tool.monitoring.svc:8080"
	type recipe struct {
		name string
		// cmd runs the recipe, its API at api.
		cmd func(api string, env ...string) *exec.Cmd
	}
	var all []recipe
	for _, r := range firingTokenRecipes(t) {
		if r.api == "" {
			continue
		}
		all = append(all, recipe{r.name, func(api string, env ...string) *exec.Cmd {
			return r.command(api, tokenFile(t, "x"), append([]string{path}, env...)...)
		}})
	}
	for _, r := range tokenRecipes() {
		all = append(all, recipe{r.script, func(api string, env ...string) *exec.Cmd {
			cmd := exec.Command(recipeShell(), recipes+r.script)
			cmd.Env = append(append(append(os.Environ(), r.env...), path), append(env, r.api+"="+api, r.token+"="+tokenFile(t, "x"))...)
			return cmd
		}})
	}
	for _, r := range trackerTokenRecipes() {
		all = append(all, recipe{r.script, func(api string, env ...string) *exec.Cmd {
			return r.command("http://lapilli:8082", tokenFile(t, "x"), api, tokenFile(t, "x"), append([]string{path}, env...)...)
		}})
	}
	for _, r := range all {
		run := func(api string, env ...string) (string, string) {
			os.Remove(args)
			out, _ := r.cmd(api, env...).CombinedOutput()
			b, _ := os.ReadFile(args)
			return string(out), string(b)
		}
		// Off, by default or false: refused before curl asks it.
		for _, off := range [][]string{nil, {"PLAIN_HTTP=false"}} {
			if said, called := run(plain, off...); !strings.Contains(said, "is not https (PLAIN_HTTP=true takes plain http, to any host)") ||
				strings.Contains(called, plain) {
				t.Errorf("%s, %v: said %q, curl asked %q", r.name, off, said, called)
			}
		}
		// On: asked, curl speaking http alone.
		said, called := run(plain, "PLAIN_HTTP=true")
		asked := ""
		for _, call := range strings.Split(called, "-q\n") {
			if strings.Contains(call, "\n"+plain) {
				asked = call
			}
		}
		if !strings.Contains(asked, "--proto\n=http\n") {
			t.Errorf("%s, PLAIN_HTTP=true: curl's arguments %q, want --proto =http and %s; said %q", r.name, asked, plain, said)
		}
		// A user before @, and another word for true.
		if said, called := run("http://ana:s3cret@tool.monitoring.svc:8080", "PLAIN_HTTP=true"); !strings.Contains(said, "holds a user or ") || strings.Contains(called, "s3cret") {
			t.Errorf("%s, a user before @: said %q, curl asked %q", r.name, said, called)
		}
		if said, called := run(plain, "PLAIN_HTTP=yes"); !strings.Contains(said, "PLAIN_HTTP is true or false") || called != "" {
			t.Errorf("%s, PLAIN_HTTP=yes: said %q, curl asked %q", r.name, said, called)
		}
	}
}
