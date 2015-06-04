package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type NinjaGenerator struct {
	f       *os.File
	nodes   []*DepNode
	vars    Vars
	exports map[string]bool
	ex      *Executor
	ruleId  int
	done    map[string]bool
	ccRe    *regexp.Regexp
}

func NewNinjaGenerator(g *DepGraph) *NinjaGenerator {
	ccRe, err := regexp.Compile(`^prebuilts/(gcc|clang)/.*(gcc|g\+\+|clang|clang\+\+) .* -c `)
	if err != nil {
		panic(err)
	}
	return &NinjaGenerator{
		nodes:   g.nodes,
		vars:    g.vars,
		exports: g.exports,
		done:    make(map[string]bool),
		ccRe:    ccRe,
	}
}

func getDepfileImpl(ss string) (string, error) {
	tss := ss + " "
	if !strings.Contains(tss, " -MD ") && !strings.Contains(tss, " -MMD ") {
		return "", nil
	}

	mfIndex := strings.Index(ss, " -MF ")
	if mfIndex >= 0 {
		mf := trimLeftSpace(ss[mfIndex+4:])
		if strings.Index(mf, " -MF ") >= 0 {
			return "", fmt.Errorf("Multiple output file candidates in %s", ss)
		}
		mfEndIndex := strings.IndexAny(mf, " \t\n")
		if mfEndIndex >= 0 {
			mf = mf[:mfEndIndex]
		}

		return mf, nil
	}

	outIndex := strings.Index(ss, " -o ")
	if outIndex < 0 {
		return "", fmt.Errorf("Cannot find the depfile in %s", ss)
	}
	out := trimLeftSpace(ss[outIndex+4:])
	if strings.Index(out, " -o ") >= 0 {
		return "", fmt.Errorf("Multiple output file candidates in %s", ss)
	}
	outEndIndex := strings.IndexAny(out, " \t\n")
	if outEndIndex >= 0 {
		out = out[:outEndIndex]
	}
	return stripExt(out) + ".d", nil
}

func getDepfile(ss string) (string, error) {
	// A hack for Android - llvm-rs-cc seems not to emit a dep file.
	if strings.Contains(ss, "bin/llvm-rs-cc ") {
		return "", nil
	}

	r, err := getDepfileImpl(ss)
	if r == "" || err != nil {
		return r, err
	}

	// A hack for Android to get .P files instead of .d.
	p := stripExt(r) + ".P"
	if strings.Contains(ss, p) {
		return p, nil
	}

	// A hack for Android. For .s files, GCC does not use
	// C preprocessor, so it ignores -MF flag.
	as := "/" + stripExt(filepath.Base(r)) + ".s"
	if strings.Contains(ss, as) {
		return "", nil
	}

	return r, nil
}

func stripShellComment(s string) string {
	if strings.IndexByte(s, '#') < 0 {
		// Fast path.
		return s
	}
	var escape bool
	var quote rune
	for i, c := range s {
		if quote > 0 {
			if quote == c && (quote == '\'' || !escape) {
				quote = 0
			}
		} else if !escape {
			if c == '#' {
				return s[:i]
			} else if c == '\'' || c == '"' || c == '`' {
				quote = c
			}
		}
		if escape {
			escape = false
		} else if c == '\\' {
			escape = true
		} else {
			escape = false
		}
	}
	return s
}

func (n *NinjaGenerator) genShellScript(runners []runner) (string, bool) {
	useGomacc := false
	var buf bytes.Buffer
	for i, r := range runners {
		if i > 0 {
			if runners[i-1].ignoreError {
				buf.WriteString(" ; ")
			} else {
				buf.WriteString(" && ")
			}
		}
		cmd := stripShellComment(r.cmd)
		cmd = trimLeftSpace(cmd)
		cmd = strings.Replace(cmd, "\\\n", " ", -1)
		cmd = strings.TrimRight(cmd, " \t\n;")
		cmd = strings.Replace(cmd, "$", "$$", -1)
		cmd = strings.Replace(cmd, "\t", " ", -1)
		if cmd == "" {
			cmd = "true"
		}
		if gomaDir != "" && n.ccRe.MatchString(cmd) {
			cmd = fmt.Sprintf("%s/gomacc %s", gomaDir, cmd)
			useGomacc = true
		}

		needsSubShell := i > 0 || len(runners) > 1
		if cmd[0] == '(' {
			needsSubShell = false
		}

		if needsSubShell {
			buf.WriteByte('(')
		}
		buf.WriteString(cmd)
		if i == len(runners)-1 && r.ignoreError {
			buf.WriteString(" ; true")
		}
		if needsSubShell {
			buf.WriteByte(')')
		}
	}
	return buf.String(), gomaDir != "" && !useGomacc
}

func (n *NinjaGenerator) genRuleName() string {
	ruleName := fmt.Sprintf("rule%d", n.ruleId)
	n.ruleId++
	return ruleName
}

func (n *NinjaGenerator) emitBuild(output, rule, dep string) {
	fmt.Fprintf(n.f, "build %s: %s%s\n", output, rule, dep)
}

func getDepString(node *DepNode) string {
	var deps []string
	var orderOnlys []string
	for _, d := range node.Deps {
		if d.IsOrderOnly {
			orderOnlys = append(orderOnlys, d.Output)
		} else {
			deps = append(deps, d.Output)
		}
	}
	dep := ""
	if len(deps) > 0 {
		dep += fmt.Sprintf(" %s", strings.Join(deps, " "))
	}
	if len(orderOnlys) > 0 {
		dep += fmt.Sprintf(" || %s", strings.Join(orderOnlys, " "))
	}
	return dep
}

func (n *NinjaGenerator) emitNode(node *DepNode) {
	if n.done[node.Output] {
		return
	}
	n.done[node.Output] = true

	if len(node.Cmds) == 0 && len(node.Deps) == 0 && !node.IsPhony {
		return
	}

	runners, _ := n.ex.createRunners(node, true)
	ruleName := "phony"
	useLocalPool := false
	if len(runners) > 0 {
		ruleName = n.genRuleName()
		fmt.Fprintf(n.f, "rule %s\n", ruleName)
		fmt.Fprintf(n.f, " description = build $out\n")

		ss, ulp := n.genShellScript(runners)
		if ulp {
			useLocalPool = true
		}
		depfile, err := getDepfile(ss)
		if err != nil {
			panic(err)
		}
		if depfile != "" {
			fmt.Fprintf(n.f, " depfile = %s\n", depfile)
		}
		// It seems Linux is OK with ~130kB.
		// TODO: Find this number automatically.
		ArgLenLimit := 100 * 1000
		if len(ss) > ArgLenLimit {
			fmt.Fprintf(n.f, " rspfile = $out.rsp\n")
			fmt.Fprintf(n.f, " rspfile_content = %s\n", ss)
			ss = "sh $out.rsp"
		}
		fmt.Fprintf(n.f, " command = %s\n", ss)

	}
	n.emitBuild(node.Output, ruleName, getDepString(node))
	if useLocalPool {
		fmt.Fprintf(n.f, " pool = local_pool\n")
	}

	for _, d := range node.Deps {
		n.emitNode(d)
	}
}

func (n *NinjaGenerator) generateShell() {
	f, err := os.Create("ninja.sh")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	ev := newEvaluator(n.vars)
	shell := ev.EvaluateVar("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	fmt.Fprintf(f, "#!%s\n", shell)
	for name, export := range n.exports {
		if export {
			fmt.Fprintf(f, "export %s=%s\n", name, ev.EvaluateVar(name))
		} else {
			fmt.Fprintf(f, "unset %s\n", name)
		}
	}
	if gomaDir == "" {
		fmt.Fprintf(f, "exec ninja\n")
	} else {
		fmt.Fprintf(f, "exec ninja -j300\n")
	}

	err = f.Chmod(0755)
	if err != nil {
		panic(err)
	}
}

func (n *NinjaGenerator) generateNinja() {
	f, err := os.Create("build.ninja")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	n.f = f
	fmt.Fprintf(n.f, "# Generated by kati\n")
	fmt.Fprintf(n.f, "\n")

	if gomaDir != "" {
		fmt.Fprintf(n.f, "pool local_pool\n")
		fmt.Fprintf(n.f, " depth = %d\n", runtime.NumCPU())
	}

	n.ex = NewExecutor(n.vars)
	for _, node := range n.nodes {
		n.emitNode(node)
	}
}

func GenerateNinja(g *DepGraph) {
	n := NewNinjaGenerator(g)
	n.generateShell()
	n.generateNinja()
}
