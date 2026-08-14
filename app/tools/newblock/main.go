// Command newblock scaffolds a training block.
//
// A block has a handful of rules that are only discoverable by tripping over
// them — it starts on a Monday, weeks are numbered from one with no gaps, every
// week has exactly seven days, and every day needs a kind and a label. The
// scaffold gets those right so the first thing you see is a plan you can edit
// rather than a load failure you have to decode.
//
// It lives in its own package under tools/ deliberately: SRC_HASH globs *.go at
// the app root and the Dockerfile copies the same, so nothing here reaches the
// image or forces a redeploy.
//
//	go run ./tools/newblock -id 2027-spring-5k -start 2027-03-01 -weeks 8
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// week is the shape the scaffold repeats: four runs, three days off. It is a
// starting point that loads, not a recommendation.
var skeleton = []struct{ kind, label, dist, mins string }{
	{"rest", "Rest", "", ""},
	{"quality", "Quality session — edit me", "6 mi", ""},
	{"rest", "Rest", "", ""},
	{"easy", "Easy run", "5 mi", ""},
	{"rest", "Rest", "", ""},
	{"long", "Long run", "8 mi", ""},
	{"recovery", "Recovery run", "4 mi", ""},
}

func main() {
	id := flag.String("id", "", "block id; also the filename")
	name := flag.String("name", "", "display name (default: derived from the id)")
	start := flag.String("start", "", "first Monday, YYYY-MM-DD")
	weeks := flag.Int("weeks", 16, "how many weeks")
	libDir := flag.String("library", "data/library", "library to scan for expected block vars")
	flag.Parse()

	if *id == "" || *start == "" {
		fail("need -id and -start")
	}
	if *weeks < 1 {
		fail("-weeks must be at least 1")
	}
	day, err := time.Parse("2006-01-02", *start)
	if err != nil {
		fail("-start %q: want YYYY-MM-DD", *start)
	}
	if day.Weekday() != time.Monday {
		// Every date in the app is start + 7·(week−1) + weekday, so this is not
		// a preference. Name the Monday rather than just refusing.
		back := (int(day.Weekday()) + 6) % 7
		fail("%s is a %s; blocks begin on a Monday — try %s or %s",
			*start, day.Weekday(),
			day.AddDate(0, 0, -back).Format("2006-01-02"),
			day.AddDate(0, 0, 7-back).Format("2006-01-02"))
	}
	if *name == "" {
		*name = titleFromID(*id)
	}
	end := day.AddDate(0, 0, *weeks*7-1)

	var b strings.Builder
	fmt.Fprintf(&b, "{\n \"schema\": \"block/1\",\n")
	fmt.Fprintf(&b, " \"id\": %q,\n", *id)
	fmt.Fprintf(&b, " \"name\": %q,\n", *name)
	fmt.Fprintf(&b, " \"title\": %q,\n", *name)
	fmt.Fprintf(&b, " \"note\": \"4 run days\",\n")
	fmt.Fprintf(&b, " \"start\": %q,\n", *start)
	fmt.Fprintf(&b, " \"goal\": {\n  \"event\": \"5K\",\n  \"date\": %q,\n  \"target\": \"00:00\"\n },\n",
		end.Format("2006-01-02"))
	fmt.Fprintf(&b, " \"intro\": \"%d weeks. Edit this standfirst.\",\n", *weeks)

	fmt.Fprintf(&b, " \"mesocycles\": [\n  { \"name\": \"Base\", \"weeks\": \"1+\" }\n ],\n")

	// The guide library is shared by every block, so a guide saying
	// {{phase "calf"}} obliges this block to declare a calf phase covering
	// every week. Stub one per issue the library asks about.
	if issues := libraryRefs(*libDir, `phaseD?e?t?a?i?l?\s+\\?"([A-Za-z][A-Za-z0-9_-]*)\\?"`); len(issues) > 0 {
		b.WriteString(" \"phases\": [\n")
		for i, is := range issues {
			fmt.Fprintf(&b, "  {\n   \"name\": \"Stage one\",\n   \"weeks\": \"1+\",\n"+
				"   \"issue\": %q,\n   \"detail\": \"TODO: what this stage asks for\"\n  }", is)
			if i < len(issues)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(" ],\n")
	}

	// Same again for block variables: {{var "easyBand"}} in a shared guide
	// obliges every block to declare easyBand, and the failure surfaces as a
	// template error deep inside that guide rather than here.
	if vars := libraryVars(*libDir); len(vars) > 0 {
		b.WriteString(" \"vars\": {\n")
		for i, v := range vars {
			fmt.Fprintf(&b, "  %q: [\n   { \"weeks\": \"1+\", \"value\": \"TODO %s\" }\n  ]", v, v)
			if i < len(vars)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(" },\n")
	}
	fmt.Fprintf(&b, " \"targets\": {\n  \"kinds\": {\n")
	fmt.Fprintf(&b, "   \"easy\": [\"HR cap {{.Athlete.HR.easyCap}}\"],\n")
	fmt.Fprintf(&b, "   \"long\": [\"HR cap {{.Athlete.HR.easyCap}}\"],\n")
	fmt.Fprintf(&b, "   \"recovery\": [\"HR cap {{.Athlete.HR.recoveryCap}}\"],\n")
	fmt.Fprintf(&b, "   \"quality\": [\"The week's one hard run.\"]\n  }\n },\n")
	fmt.Fprintf(&b, " \"checklist\": [\n  {\n   \"key\": \"session\",\n"+
		"   \"label\": \"{{.Session.Label}}\",\n   \"guide\": \"{{sessionGuide}}\",\n"+
		"   \"when\": \"{{and .InBlock (not .Resting)}}\"\n  }\n ],\n")

	fmt.Fprintf(&b, " \"weeks\": [\n")
	for w := 1; w <= *weeks; w++ {
		fmt.Fprintf(&b, "  {\n   \"n\": %d,\n   \"days\": [\n", w)
		for i, d := range skeleton {
			fmt.Fprintf(&b, "    {\n     \"kind\": %q,\n     \"label\": %q", d.kind, d.label)
			if d.dist != "" {
				fmt.Fprintf(&b, ",\n     \"dist\": %q", d.dist)
			}
			if d.mins != "" {
				fmt.Fprintf(&b, ",\n     \"mins\": %s", d.mins)
			}
			b.WriteString("\n    }")
			if i < len(skeleton)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("   ]\n  }")
		if w < *weeks {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(" ]\n}\n")

	fmt.Print(b.String())
	fmt.Fprintf(os.Stderr, "block         %s · %d weeks · %s → %s\n",
		*id, *weeks, *start, end.Format("2006-01-02"))
	fmt.Fprintf(os.Stderr, "next          edit the sessions, then: make validate && make artifacts\n")
}

// libraryVars is every block variable the guide library expects, sorted. A
// block that does not declare one fails to load with the error pointing at the
// guide rather than at the block that is actually missing something.
func libraryVars(dir string) []string {
	return libraryRefs(dir, `var\s+\\?"([A-Za-z][A-Za-z0-9_]*)\\?"`)
}

// libraryRefs collects the distinct first capture of a pattern across the
// library, sorted.
func libraryRefs(dir, pattern string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	re := regexp.MustCompile(pattern)
	seen := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// titleFromID turns "2027-spring-5k" into "Spring 5k", dropping a leading date.
func titleFromID(id string) string {
	parts := strings.Split(id, "-")
	for len(parts) > 0 && isNumeric(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return id
	}
	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	return strings.Join(parts, " ")
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "newblock: "+format+"\n", a...)
	os.Exit(1)
}
