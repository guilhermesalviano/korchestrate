// Plan loading from user-written documents: markdown plans are parsed with
// section-heading heuristics into the Plan contract, JSON plans are read
// verbatim, and @file mentions in a prompt reference either. Pointing a run at
// plan files skips the planner stage.

package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// LoadPlanFile reads a plan from path. .json files must satisfy the plan
// contract; .md files are parsed as markdown plans; anything else is sniffed.
func LoadPlanFile(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return decodePlanJSON(path, data)
	case ".md", ".markdown":
		return PlanFromMarkdown(path, data)
	default:
		if plan, err := decodePlanJSON(path, data); err == nil {
			return plan, nil
		}
		return PlanFromMarkdown(path, data)
	}
}

// LoadPlans reads and merges plan files in order. Steps are renumbered and
// lists concatenated; the first non-empty summary wins.
func LoadPlans(paths []string) (*Plan, error) {
	var merged *Plan
	for _, p := range paths {
		plan, err := LoadPlanFile(p)
		if err != nil {
			return nil, err
		}
		merged = mergePlans(merged, plan)
	}
	if merged == nil {
		return nil, errors.New("no plan files given")
	}
	if err := merged.Validate(); err != nil {
		return nil, err
	}
	return merged, nil
}

func decodePlanJSON(path string, data []byte) (*Plan, error) {
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan %s: %w", path, err)
	}
	return &plan, nil
}

func mergePlans(a, b *Plan) *Plan {
	if a == nil {
		return b
	}
	if strings.TrimSpace(a.Summary) == "" {
		a.Summary = b.Summary
	}
	a.Assumptions = append(a.Assumptions, b.Assumptions...)
	a.Files = append(a.Files, b.Files...)
	a.Steps = append(a.Steps, b.Steps...)
	a.AcceptanceCriteria = append(a.AcceptanceCriteria, b.AcceptanceCriteria...)
	a.OutOfScope = append(a.OutOfScope, b.OutOfScope...)
	for i := range a.Steps {
		a.Steps[i].ID = strconv.Itoa(i + 1)
	}
	return a
}

var mentionRe = regexp.MustCompile(`(?:^|[\s(\[{])@([^\s@]+\.(?:md|markdown|json))`)

// ExtractPlanFiles returns the @-mentioned plan files (.md/.json) in prompt
// and the prompt with the mentions stripped. A mention must start a word, so
// email addresses do not match.
func ExtractPlanFiles(prompt string) (paths []string, rest string) {
	seen := map[string]bool{}
	rest = mentionRe.ReplaceAllStringFunc(prompt, func(m string) string {
		i := strings.IndexByte(m, '@')
		p := strings.Trim(m[i+1:], `.,;:!?)'"`)
		if p == "" || seen[p] {
			return m[:i]
		}
		seen[p] = true
		paths = append(paths, p)
		return m[:i]
	})
	lines := strings.Split(rest, "\n")
	for i := range lines {
		lines[i] = strings.Join(strings.Fields(lines[i]), " ")
	}
	rest = strings.TrimSpace(strings.Join(lines, "\n"))
	return paths, rest
}

// --- markdown parsing ---------------------------------------------------------

type mdBlock struct {
	text string
}

type mdSection struct {
	title string // normalized heading text
	items []mdBlock
	paras []string
}

type mdDoc struct {
	title string // leading H1, if any
	intro mdSection
	secs  []*mdSection
}

type sectionKind int

const (
	secOther sectionKind = iota
	secSummary
	secAssumptions
	secFiles
	secSteps
	secAccept
	secOut
)

var (
	headingRe  = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	listItemRe = regexp.MustCompile(`^(\s*)(?:[-*+]|\d{1,3}[.)])\s+(.*)$`)
	checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s+`)
	codeSpanRe = regexp.MustCompile("`([^`]+)`")
	setextH1Re = regexp.MustCompile(`^=+\s*$`)
	boldEdgeRe = regexp.MustCompile(`\*\*`)
)

// kindKeys maps normalized heading prefixes to section kinds. Acceptance
// headings are matched first so "how to verify" is not read as a step "how".
var kindKeys = []struct {
	kind sectionKind
	keys []string
}{
	{secAccept, []string{"acceptance criteria", "acceptance", "criteria", "definition of done", "success criteria", "done when", "verification", "verify", "how to verify", "validation", "validate"}},
	{secOut, []string{"out of scope", "non goals", "non-goals", "non-goals:", "not in scope"}},
	{secFiles, []string{"files", "file list", "files to touch", "touched files", "scope files"}},
	{secAssumptions, []string{"assumptions", "assumption"}},
	{secSummary, []string{"summary", "overview", "goal", "goals", "purpose", "objective", "objectives", "context", "background", "description", "intent"}},
	{secSteps, []string{"steps", "step by step", "implementation", "implementation plan", "implementation steps", "plan", "tasks", "task list", "changes", "change list", "procedure", "approach", "how"}},
}

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "`", "")
	s = strings.TrimSpace(s)
	for _, suf := range []string{":", "-", "="} {
		s = strings.TrimSuffix(s, suf)
	}
	return strings.Join(strings.Fields(s), " ")
}

func classify(title string) sectionKind {
	for _, g := range kindKeys {
		for _, k := range g.keys {
			k = normalizeTitle(k)
			if title == k || strings.HasPrefix(title, k+" ") {
				return g.kind
			}
		}
	}
	return secOther
}

// parseMarkdown splits a document into a title, the leading intro section and
// heading-delimited sections of list items and paragraphs. Fenced code blocks
// are skipped so their contents cannot be mistaken for steps.
func parseMarkdown(src []byte) (doc mdDoc) {
	cur := &doc.intro
	inFence := false
	var para []string

	flush := func() {
		if len(para) > 0 {
			cur.paras = append(cur.paras, strings.Join(para, " "))
			para = nil
		}
	}
	appendItem := func(indent int, text string) {
		if indent > 0 && len(cur.items) > 0 {
			cur.items[len(cur.items)-1].text = strings.TrimSpace(cur.items[len(cur.items)-1].text + " " + text)
		} else {
			cur.items = append(cur.items, mdBlock{text: text})
		}
	}

	lines := strings.Split(string(src), "\n")
	for n, line := range lines {
		l := strings.TrimRight(line, " \t\r")
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			flush()
			inFence = !inFence
			continue
		}
		if inFence || t == "" {
			flush()
			continue
		}
		if h := headingRe.FindStringSubmatch(l); h != nil {
			flush()
			if doc.title == "" && strings.HasPrefix(l, "# ") && len(doc.secs) == 0 && len(doc.intro.items) == 0 && len(doc.intro.paras) == 0 {
				doc.title = strings.TrimSpace(h[1])
				continue
			}
			cur = &mdSection{title: normalizeTitle(h[1])}
			doc.secs = append(doc.secs, cur)
			continue
		}
		// Setext H1: a line of "=" underlining the previous text line.
		if setextH1Re.MatchString(l) && n > 0 && doc.title == "" && len(doc.secs) == 0 {
			if len(para) == 1 {
				doc.title = para[0]
				para = nil
			}
			continue
		}
		if m := listItemRe.FindStringSubmatch(l); m != nil {
			flush()
			indent := len(strings.ReplaceAll(m[1], "\t", "  ")) / 2
			appendItem(indent, checkboxRe.ReplaceAllString(m[2], ""))
			continue
		}
		// Indented text continues the previous list item, else a paragraph.
		if l != t && len(cur.items) > 0 && len(para) == 0 {
			appendItem(1, t)
			continue
		}
		para = append(para, t)
	}
	flush()
	return doc
}

// PlanFromMarkdown converts a markdown document into a Plan using its section
// headings: summary/overview, assumptions, files, steps/tasks/implementation,
// acceptance criteria / definition of done and out of scope are recognized.
// Lists become steps or criteria; when a section is missing sensible fallbacks
// keep the plan contract valid. name is the source file, used in fallbacks
// and errors.
func PlanFromMarkdown(name string, src []byte) (*Plan, error) {
	doc := parseMarkdown(src)
	display := filepath.Base(name)
	plan := &Plan{}

	if s := firstSection(doc, secSummary); s != nil {
		plan.Summary = sectionText(s)
	}
	if plan.Summary == "" {
		plan.Summary = firstParagraph(doc)
	}
	if plan.Summary == "" {
		plan.Summary = cleanInline(doc.title)
	}
	if plan.Summary == "" {
		plan.Summary = display
	}

	for _, s := range doc.secs {
		switch classify(s.title) {
		case secAssumptions:
			plan.Assumptions = append(plan.Assumptions, sectionLines(s)...)
		case secFiles:
			for _, f := range sectionLines(s) {
				if spans := pathSpans(f); len(spans) > 0 {
					plan.Files = append(plan.Files, spans...)
				} else {
					plan.Files = append(plan.Files, f)
				}
			}
		case secOut:
			plan.OutOfScope = append(plan.OutOfScope, sectionLines(s)...)
		}
	}

	plan.Steps = stepsFrom(doc)
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("no steps found in %s: add a numbered or bulleted list under a Steps/Implementation heading", name)
	}
	for i := range plan.Steps {
		plan.Steps[i].ID = strconv.Itoa(i + 1)
	}

	for _, s := range doc.secs {
		if classify(s.title) != secAccept {
			continue
		}
		plan.AcceptanceCriteria = append(plan.AcceptanceCriteria, sectionLines(s)...)
	}
	if len(plan.AcceptanceCriteria) == 0 {
		plan.AcceptanceCriteria = []string{fmt.Sprintf("Every step of %s is implemented as described", display)}
	}

	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("invalid plan %s: %w", name, err)
	}
	return plan, nil
}

// firstSection returns the first section classified as kind.
func firstSection(doc mdDoc, kind sectionKind) *mdSection {
	for _, s := range doc.secs {
		if classify(s.title) == kind {
			return s
		}
	}
	return nil
}

// sectionText returns a section's first paragraph or item.
func sectionText(s *mdSection) string {
	if len(s.paras) > 0 {
		return cleanInline(s.paras[0])
	}
	if len(s.items) > 0 {
		return cleanInline(s.items[0].text)
	}
	return ""
}

// sectionLines lists a section's items, falling back to its paragraphs.
func sectionLines(s *mdSection) []string {
	var out []string
	for _, it := range s.items {
		if t := cleanInline(it.text); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		for _, p := range s.paras {
			if t := cleanInline(p); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// stepsFrom derives plan steps: list items of step-classified sections, then
// any list in the document, then intro paragraphs. Paragraph-only step
// sections contribute one step per paragraph.
func stepsFrom(doc mdDoc) []PlanStep {
	var steps []PlanStep
	add := func(text string) {
		if t := cleanInline(text); t != "" {
			steps = append(steps, PlanStep{Description: t, Files: pathSpans(text)})
		}
	}
	for _, s := range doc.secs {
		if classify(s.title) != secSteps {
			continue
		}
		for _, it := range s.items {
			add(it.text)
		}
		for _, p := range s.paras {
			add(p)
		}
	}
	if len(steps) > 0 {
		return steps
	}
	for _, it := range doc.intro.items {
		add(it.text)
	}
	for _, s := range doc.secs {
		for _, it := range s.items {
			add(it.text)
		}
	}
	if len(steps) > 0 {
		return steps
	}
	for _, p := range doc.intro.paras {
		add(p)
	}
	return steps
}

// firstParagraph returns the document's first paragraph, preferring the intro.
func firstParagraph(doc mdDoc) string {
	if len(doc.intro.paras) > 0 {
		return cleanInline(doc.intro.paras[0])
	}
	for _, s := range doc.secs {
		if len(s.paras) > 0 {
			return cleanInline(s.paras[0])
		}
	}
	return ""
}

// cleanInline strips emphasis markers and trailing label colons from text.
func cleanInline(s string) string {
	s = boldEdgeRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ":")
	return strings.TrimSpace(s)
}

// pathSpans extracts backticked spans that look like file paths.
func pathSpans(text string) []string {
	var out []string
	for _, m := range codeSpanRe.FindAllStringSubmatch(text, -1) {
		if s := m[1]; looksLikePath(s) {
			out = append(out, s)
		}
	}
	return out
}

func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") || strings.Contains(s, "://") {
		return false
	}
	if strings.HasPrefix(s, "-") {
		return false
	}
	return strings.ContainsAny(s, "./")
}

// Markdown renders the plan as a document PlanFromMarkdown reads back, so the
// saved plan.md can be handed to the executor or edited before resuming.
func (p *Plan) Markdown() string {
	var b strings.Builder
	b.WriteString("# Implementation plan\n\n## Summary\n\n")
	b.WriteString(strings.TrimSpace(p.Summary) + "\n")
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, item := range items {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(item))
		}
	}
	list("Assumptions", p.Assumptions)
	files := make([]string, len(p.Files))
	for i, f := range p.Files {
		files[i] = "`" + f + "`"
	}
	list("Files", files)
	b.WriteString("\n## Steps\n\n")
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(s.Description))
		if len(s.Files) > 0 {
			fmt.Fprintf(&b, "   - Files: `%s`\n", strings.Join(s.Files, "`, `"))
		}
		if v := strings.TrimSpace(s.Verification); v != "" {
			fmt.Fprintf(&b, "   - Check: %s\n", v)
		}
	}
	list("Acceptance criteria", p.AcceptanceCriteria)
	list("Out of scope", p.OutOfScope)
	return b.String()
}
