// Command fakeoutsidepi installs an isolated npm-shaped Pi fixture and execs
// Node, so discovery sees the same kernel evidence as a terminal Pi launch.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const terminal = `#!/usr/bin/env node
import readline from 'node:readline';
import {spawn} from 'node:child_process';
if (process.argv.includes('--mode') || process.argv.includes('--list-models')) {
  const agent = spawn(process.env.HOME + '/.local/bin/pi', process.argv.slice(2), {stdio:'inherit'});
  agent.once('exit', code => process.exit(code ?? 1));
} else {
if (process.argv.includes('--title')) process.title = 'pi';
const emit = x => process.stdout.write(JSON.stringify(x) + '\n');
let child;
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => {
  emit({event:'signal', signal:sig, at:new Date().toISOString()});
  if (sig === 'SIGTERM') {
    if (child) { child.once('exit', () => process.exit(0)); child.kill(); }
    else process.exit(0);
  }
});
readline.createInterface({input:process.stdin}).on('line', line => {
  if (line === 'barrier') setImmediate(() => emit({event:'barrier'}));
  if (line.startsWith('cwd ')) { process.chdir(line.slice(4)); emit({event:'cwd'}); }
  if (line === 'child' && !child) {
    child = spawn('/bin/sleep', ['600'], {stdio:'ignore'});
    child.once('spawn', () => emit({event:'child_ready', pid:child.pid}));
    child.once('exit', () => { child = undefined; emit({event:'child_exit'}); });
  }
  if (line === 'stop-child' && child) child.kill();
});
emit({event:'ready'});
}
`

func main() {
	if len(os.Args) < 2 {
		panic("package root required")
	}
	root := os.Args[1]
	node, err := exec.LookPath("node")
	if err != nil {
		panic(err)
	}
	script := terminal
	var args []string
	for _, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, "--node=") {
			node = strings.TrimPrefix(arg, "--node=")
			script = strings.Replace(script, "#!/usr/bin/env node", "#!"+node, 1)
		} else {
			args = append(args, arg)
		}
	}
	pkg := filepath.Join(root, "node_modules", "@earendil-works", "pi-coding-agent")
	cli := filepath.Join(pkg, "dist", "bundle", "cli.js")
	if err := os.MkdirAll(filepath.Dir(cli), 0o700); err != nil {
		panic(err)
	}
	for path, data := range map[string]string{
		filepath.Join(pkg, "package.json"): `{"name":"@earendil-works/pi-coding-agent","version":"1.0.0","type":"module","bin":{"pi":"dist/bundle/cli.js"}}`,
		cli:                                script,
	} {
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			panic(err)
		}
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		panic(err)
	}
	entry := filepath.Join(bin, "pi")
	if err := os.Symlink(cli, entry); err != nil {
		panic(err)
	}
	if err := syscall.Exec(node, append([]string{node, entry}, args...), os.Environ()); err != nil {
		panic(fmt.Sprint(err))
	}
}
