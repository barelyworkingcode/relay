package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

func (e *env) audit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	tail := fs.Int("tail", 50, "")
	project := fs.String("project", "", "")
	event := fs.String("event", "", "")
	outcome := fs.String("outcome", "", "")
	asJSON := fs.Bool("json", false, "")
	path := fs.Bool("path", false, "")
	if code, ok := e.parse(fs, args); !ok {
		return code
	}
	file := filepath.Join(e.g.ConfigDir, "logs", "audit", "toolcalls.jsonl")
	if *path {
		fmt.Fprintln(e.out, file)
		return 0
	}
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		return e.fail(2, "%v", err)
	}
	defer f.Close()
	type row struct {
		raw string
		m   map[string]any
	}
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		actor, _ := m["actor"].(map[string]any)
		pid, _ := actor["project_id"].(string)
		if (*project != "" && pid != *project) || (*event != "" && m["event"] != *event) || (*outcome != "" && m["outcome"] != *outcome) {
			continue
		}
		rows = append(rows, row{sc.Text(), m})
	}
	if *tail > 0 && len(rows) > *tail {
		rows = rows[len(rows)-*tail:]
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		if *asJSON {
			fmt.Fprintln(e.out, r.raw)
			continue
		}
		actor, _ := r.m["actor"].(map[string]any)
		ts, _ := r.m["ts"].(string)
		fmt.Fprintf(tw, "%s\t%v\t%v\t%v\t%v\n", ts, r.m["outcome"], dash(actor["project_id"]), r.m["event"], strings.TrimSpace(str(r.m["tool"])+" "+str(r.m["error"])))
	}
	if !*asJSON {
		if len(rows) == 0 {
			fmt.Fprintln(e.out, "no audit records")
		}
		tw.Flush()
	}
	return 0
}

func dash(v any) any {
	if s, ok := v.(string); !ok || s == "" {
		return "-"
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
