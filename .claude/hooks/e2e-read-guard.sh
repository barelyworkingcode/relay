#!/bin/bash
# PreToolUse guard for the e2e-test-writer agent: it must write feature tests
# from the docs and the harness, never from relay's own code. This stops
# accidental reads. It is not a security boundary: a determined shell command
# can always find another way to read a file.
#
# Input is the hook JSON on stdin. Exit 2 with one line on stderr refuses the
# tool call; exit 0 allows it.

RELAY_MODULE="github.com/barelyworkingcode/relay"

refuse() {
	echo "e2e-read-guard: $1" >&2
	exit 2
}

if ! command -v jq >/dev/null 2>&1; then
	refuse "jq is required to inspect the tool call; install it or run the agent elsewhere"
fi

input=$(cat)
tool=$(jq -r '.tool_name // ""' <<<"$input")
cwd=$(jq -r '.cwd // ""' <<<"$input")
[ -n "$cwd" ] || cwd=$PWD

# resolve prints an absolute, symlink-free path. The tail of a path that does
# not exist yet is appended to its deepest existing ancestor, so a file the
# agent is about to create cannot slip under cmd/ through a symlinked parent.
resolve() {
	local p=$1 rest="" base
	case "$p" in
	/*) ;;
	*) p="$cwd/$p" ;;
	esac
	while [ ! -e "$p" ] && [ "$p" != "/" ]; do
		base=${p##*/}
		rest="/$base$rest"
		p=${p%/*}
		[ -n "$p" ] || p=/
	done
	local real
	real=$(cd "$p" 2>/dev/null && pwd -P) || real=$p
	if [ -f "$p" ]; then
		real="$(cd "${p%/*}" 2>/dev/null && pwd -P)/${p##*/}"
	fi
	# Collapse any "." or ".." left in the non-existent tail.
	local out="$real$rest" parts=() seg
	local IFS=/
	for seg in $out; do
		case "$seg" in
		"" | .) ;;
		..) [ "${#parts[@]}" -gt 0 ] && unset 'parts[${#parts[@]}-1]' ;;
		*) parts+=("$seg") ;;
		esac
	done
	echo "/${parts[*]}"
}

is_relay_root() {
	[ -f "$1/go.mod" ] || return 1
	[ "$(awk '/^module[[:space:]]/ { print $2; exit }' "$1/go.mod")" = "$RELAY_MODULE" ]
}

# inside_guarded_dir succeeds when the path is, or lies under, a cmd/ or
# internal/ directory of relay's module root.
inside_guarded_dir() {
	local d=$1 name
	while [ -n "$d" ] && [ "$d" != "/" ]; do
		name=${d##*/}
		if [ "$name" = cmd ] || [ "$name" = internal ]; then
			is_relay_root "${d%/*}" && return 0
		fi
		d=${d%/*}
	done
	return 1
}

# relay_roots lists the relay module roots visible from the project dir and
# the working directory, each walked upward.
relay_roots() {
	local start d
	for start in "${CLAUDE_PROJECT_DIR:-}" "$cwd"; do
		[ -n "$start" ] || continue
		d=$(resolve "$start")
		while [ -n "$d" ] && [ "$d" != "/" ]; do
			is_relay_root "$d" && echo "$d"
			d=${d%/*}
		done
	done | sort -u
}

# covers_relay_root succeeds when the search root is a relay module root or one
# of its ancestors, so a search from there would walk cmd/ and internal/.
covers_relay_root() {
	local s=$1 r
	while read -r r; do
		[ -n "$r" ] || continue
		case "$r/" in
		"${s%/}/"*) return 0 ;;
		esac
	done < <(relay_roots)
	return 1
}

path_re='(^|[^[:alnum:]_.-])(\.\./|\./)*(cmd|internal)/'

check_bash() {
	local cmd=$1
	if [[ $cmd =~ $path_re ]]; then
		refuse "a path under cmd/ or internal/ is off limits; read docs/ or e2e/ instead"
	fi
	local segment words w i sub n
	while IFS= read -r segment; do
		# shellcheck disable=SC2206
		words=($segment)
		n=${#words[@]}
		for ((i = 0; i < n; i++)); do
			w=${words[i]}
			if [ "$w" = git ] || [[ $w == */git ]]; then
				check_git "${words[@]:i+1}"
				break
			fi
			if [ "$w" = go ] && [ "${words[i + 1]:-}" = doc ]; then
				local rest="${words[*]:i+2}"
				if [[ $rest =~ barelyworkingcode/relay([^[:alnum:]_-]|$) ]]; then
					refuse "go doc on a relay package is off limits; use go doc relaye2e/harness"
				fi
				break
			fi
		done
	done < <(printf '%s\n' "$cmd" | sed -E 's/(&&|\|\||;|\|)/\n/g')
}

check_git() {
	local args=("$@") sub="" i skip=0 a
	for ((i = 0; i < ${#args[@]}; i++)); do
		a=${args[i]}
		if [ "$skip" = 1 ]; then
			skip=0
			continue
		fi
		case "$a" in
		-C | -c | --git-dir | --work-tree | --namespace) skip=1 ;;
		-*) ;;
		*)
			sub=$a
			args=("${args[@]:i+1}")
			break
			;;
		esac
	done
	case "$sub" in
	grep | show | blame | cat-file | archive)
		refuse "git $sub reads relay's code; read docs/ or e2e/ instead"
		;;
	log)
		for a in "${args[@]}"; do
			if [[ $a == --patch* ]] || [[ $a =~ ^-[a-zA-Z]*[pu][a-zA-Z]*$ ]]; then
				refuse "git log with a patch reads relay's code; drop -p or --patch"
			fi
		done
		;;
	diff)
		local seen=0 ok=1
		for a in "${args[@]}"; do
			if [ "$seen" = 1 ]; then
				case "$a" in
				e2e | e2e/* | docs | docs/*) ;;
				*) ok=0 ;;
				esac
			elif [ "$a" = -- ]; then
				seen=1
			fi
		done
		if [ "$seen" = 0 ] || [ "$ok" = 0 ]; then
			refuse "git diff needs a pathspec limited to e2e/ or docs/ after --"
		fi
		;;
	esac
}

case "$tool" in
Read | Write | Edit | MultiEdit | NotebookEdit)
	p=$(jq -r '.tool_input.file_path // .tool_input.notebook_path // .tool_input.path // ""' <<<"$input")
	[ -n "$p" ] || exit 0
	if inside_guarded_dir "$(resolve "$p")"; then
		refuse "files under relay's cmd/ and internal/ are off limits; read docs/ or e2e/ instead"
	fi
	;;
Glob | Grep)
	p=$(jq -r '.tool_input.path // ""' <<<"$input")
	[ -n "$p" ] || p=$cwd
	root=$(resolve "$p")
	if inside_guarded_dir "$root"; then
		refuse "relay's cmd/ and internal/ are off limits; search e2e/ or docs/"
	fi
	if covers_relay_root "$root"; then
		refuse "this search root would walk relay's cmd/ and internal/; search e2e/ or docs/"
	fi
	if [ "$tool" = Glob ]; then
		pat=$(jq -r '.tool_input.pattern // ""' <<<"$input")
		if [[ $pat =~ $path_re ]]; then
			refuse "a glob naming cmd/ or internal/ is off limits; search e2e/ or docs/"
		fi
	fi
	;;
Bash)
	check_bash "$(jq -r '.tool_input.command // ""' <<<"$input")"
	;;
esac
exit 0
