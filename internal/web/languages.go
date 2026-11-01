package web

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"
)

// Language breakdown of a directory, in the spirit of GitHub's Linguist:
// bytes of code per language, detected by file name. Data, prose and
// configuration (JSON, YAML, Markdown, …) and vendored or generated paths
// don't count, so the bar shows what the code is written in.

type language struct {
	name  string
	color string
}

var languagesByExt = map[string]language{}
var languagesByName = map[string]language{}

func init() {
	for _, l := range []struct {
		name, color string
		exts        []string
	}{
		{"Go", "#00add8", []string{"go"}},
		{"Python", "#3572a5", []string{"py", "pyi", "pyw", "pyx"}},
		{"JavaScript", "#f1e05a", []string{"js", "mjs", "cjs", "jsx"}},
		{"TypeScript", "#3178c6", []string{"ts", "mts", "cts", "tsx"}},
		{"HTML", "#e34c26", []string{"html", "htm", "xhtml"}},
		{"CSS", "#663399", []string{"css"}},
		{"SCSS", "#c6538c", []string{"scss"}},
		{"Sass", "#a53b70", []string{"sass"}},
		{"Less", "#1d365d", []string{"less"}},
		{"Vue", "#41b883", []string{"vue"}},
		{"Svelte", "#ff3e00", []string{"svelte"}},
		{"Java", "#b07219", []string{"java"}},
		{"Kotlin", "#a97bff", []string{"kt", "kts"}},
		{"Scala", "#c22d40", []string{"scala", "sc"}},
		{"Groovy", "#4298b8", []string{"groovy", "gradle"}},
		{"Clojure", "#db5855", []string{"clj", "cljs", "cljc", "edn"}},
		{"C", "#555555", []string{"c", "h"}},
		{"C++", "#f34b7d", []string{"cpp", "cc", "cxx", "c++", "hpp", "hh", "hxx", "h++", "ipp"}},
		{"C#", "#178600", []string{"cs", "csx"}},
		{"F#", "#b845fc", []string{"fs", "fsi", "fsx"}},
		{"Objective-C", "#438eff", []string{"m"}},
		{"Objective-C++", "#6866fb", []string{"mm"}},
		{"Swift", "#f05138", []string{"swift"}},
		{"Rust", "#dea584", []string{"rs"}},
		{"Zig", "#ec915c", []string{"zig"}},
		{"Nim", "#ffc200", []string{"nim"}},
		{"D", "#ba595e", []string{"d"}},
		{"Ruby", "#701516", []string{"rb", "rake", "gemspec"}},
		{"PHP", "#4f5d95", []string{"php", "phtml"}},
		{"Perl", "#0298c3", []string{"pl", "pm"}},
		{"Lua", "#000080", []string{"lua"}},
		{"R", "#198ce7", []string{"r"}},
		{"Julia", "#a270ba", []string{"jl"}},
		{"Dart", "#00b4ab", []string{"dart"}},
		{"Elixir", "#6e4a7e", []string{"ex", "exs"}},
		{"Erlang", "#b83998", []string{"erl", "hrl"}},
		{"Haskell", "#5e5086", []string{"hs", "lhs"}},
		{"OCaml", "#ef7a08", []string{"ml", "mli"}},
		{"Elm", "#60b5cc", []string{"elm"}},
		{"Shell", "#89e051", []string{"sh", "bash", "zsh", "ksh"}},
		{"Fish", "#4aae47", []string{"fish"}},
		{"PowerShell", "#012456", []string{"ps1", "psm1", "psd1"}},
		{"Batchfile", "#c1f12e", []string{"bat", "cmd"}},
		{"PLpgSQL", "#336790", []string{"pgsql", "plpgsql"}},
		{"TSQL", "#e38c00", []string{"tsql"}},
		{"Solidity", "#aa6746", []string{"sol"}},
		{"HCL", "#844fba", []string{"tf", "hcl", "tfvars"}},
		{"Nix", "#7e7eff", []string{"nix"}},
		{"Starlark", "#76d275", []string{"bzl", "star"}},
		{"Protocol Buffer", "#4285f4", []string{"proto"}},
		{"Thrift", "#d12127", []string{"thrift"}},
		{"Jupyter Notebook", "#da5b0b", []string{"ipynb"}},
		{"Assembly", "#6e4c13", []string{"asm", "s"}},
		{"WebAssembly", "#04133b", []string{"wat", "wast"}},
		{"CMake", "#da3434", []string{"cmake"}},
		{"Makefile", "#427819", []string{"mk", "mak"}},
		{"Dockerfile", "#384d54", []string{"dockerfile"}},
		{"Smarty", "#f0c040", []string{"tpl"}},
		{"Jinja", "#a52a22", []string{"j2", "jinja", "jinja2"}},
		{"Handlebars", "#f7931e", []string{"hbs", "handlebars"}},
		{"Pug", "#a86454", []string{"pug", "jade"}},
		{"GLSL", "#5686a5", []string{"glsl", "vert", "frag"}},
		{"HLSL", "#aace60", []string{"hlsl"}},
		{"Vim Script", "#199f4b", []string{"vim"}},
		{"Emacs Lisp", "#c065db", []string{"el"}},
		{"Common Lisp", "#3fb68b", []string{"lisp", "cl"}},
		{"Scheme", "#1e4aec", []string{"scm", "ss"}},
		{"Racket", "#3c5caa", []string{"rkt"}},
		{"Fortran", "#4d41b1", []string{"f", "f90", "f95", "f03", "for"}},
		{"COBOL", "#004c99", []string{"cob", "cbl"}},
		{"Pascal", "#e3f171", []string{"pas", "pp"}},
		{"Ada", "#02f88c", []string{"adb", "ads"}},
		{"Verilog", "#b2b7f8", []string{"v"}},
		{"SystemVerilog", "#dae1c2", []string{"sv", "svh"}},
		{"VHDL", "#adb2cb", []string{"vhd", "vhdl"}},
		{"Crystal", "#000100", []string{"cr"}},
		{"Gleam", "#ffaff3", []string{"gleam"}},
		{"V", "#4f87c4", []string{"vv"}},
		{"Astro", "#ff5a03", []string{"astro"}},
		{"MDX", "#fcb32c", []string{"mdx"}},
		{"Cuda", "#3a4e3a", []string{"cu", "cuh"}},
		{"AutoHotkey", "#6594b9", []string{"ahk"}},
	} {
		for _, e := range l.exts {
			languagesByExt[e] = language{l.name, l.color}
		}
	}
	for name, ext := range map[string]string{
		"Dockerfile": "dockerfile", "Containerfile": "dockerfile", "Makefile": "mk", "GNUmakefile": "mk",
		"makefile": "mk", "CMakeLists.txt": "cmake", "Rakefile": "rb", "Gemfile": "rb", "BUILD": "bzl",
		"BUILD.bazel": "bzl", "WORKSPACE": "bzl", "Jenkinsfile": "groovy", "Vagrantfile": "rb",
	} {
		languagesByName[name] = languagesByExt[ext]
	}
}

// vendoredDirs are directories whose files are third-party or generated.
var vendoredDirs = map[string]bool{
	"vendor": true, "node_modules": true, "third_party": true, "thirdparty": true, "3rdparty": true,
	"bower_components": true, "dist": true, "__pycache__": true,
	".venv": true, "venv": true, ".yarn": true, "Pods": true, "Carthage": true,
}

// fileLanguage returns the language of a file, or false if it doesn't count.
func fileLanguage(p string) (language, bool) {
	dir, name := path.Split(p)
	for _, seg := range strings.Split(strings.TrimSuffix(dir, "/"), "/") {
		if vendoredDirs[seg] {
			return language{}, false
		}
	}
	if l, ok := languagesByName[name]; ok {
		return l, true
	}
	if strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile") {
		return languagesByExt["dockerfile"], true
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, ".min.") || strings.HasSuffix(lower, ".pb.go") || strings.HasSuffix(lower, "_pb2.py") ||
		strings.HasSuffix(lower, ".d.ts") || strings.HasSuffix(lower, ".bundle.js") {
		return language{}, false
	}
	ext := strings.TrimPrefix(path.Ext(name), ".")
	if ext == "" {
		return language{}, false
	}
	// Extensions are case-sensitive where case tells languages apart.
	if l, ok := languagesByExt[ext]; ok {
		return l, true
	}
	l, ok := languagesByExt[strings.ToLower(ext)]
	return l, ok
}

type langStat struct {
	Name    string
	Color   string
	Bytes   int64
	Percent float64
}

// minLangPercent hides languages below it from the legend; they are
// merged into "Other" so the bar still adds up.
const minLangPercent = 0.1

// languageStats returns the languages of a directory at a commit, biggest
// first. It is cached in Redis by tree id, so an unchanged directory is
// counted once whatever commit it is viewed at.
func (w *Web) languageStats(ctx context.Context, commit, dir string) []langStat {
	tree, err := w.Repo.TreeID(ctx, commit, dir)
	if err != nil {
		return nil
	}
	key := "languages:" + tree
	var stats []langStat
	if ok, err := w.KV.GetJSON(ctx, key, &stats); ok && err == nil {
		return stats
	}
	// The page is useful without the bar: don't wait long for a slot.
	release, err := w.acquireHeavy(ctx, 2*time.Second)
	if err != nil {
		return nil
	}
	defer release()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sizes := map[language]int64{}
	var total int64
	err = w.Repo.WalkBlobs(wctx, tree, func(p string, size int64) {
		if l, ok := fileLanguage(p); ok {
			sizes[l] += size
			total += size
		}
	})
	if err != nil || wctx.Err() != nil {
		return nil
	}
	stats = []langStat{}
	var other int64
	for l, n := range sizes {
		pct := float64(n) * 100 / float64(total)
		if pct < minLangPercent {
			other += n
			continue
		}
		stats = append(stats, langStat{Name: l.name, Color: l.color, Bytes: n, Percent: pct})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Bytes != stats[j].Bytes {
			return stats[i].Bytes > stats[j].Bytes
		}
		return stats[i].Name < stats[j].Name
	})
	if other > 0 {
		stats = append(stats, langStat{Name: "Other", Color: "#8a8f98", Bytes: other, Percent: float64(other) * 100 / float64(total)})
	}
	if err := w.KV.SetJSON(ctx, key, stats, 30*24*time.Hour); err != nil {
		w.Log.Warn("cache languages", "err", err)
	}
	return stats
}
