package main

import (
	"fmt"
	"strings"
)

var wifiSubs = []string{"enable", "connect", "pair", "scan", "disconnect", "usb"}

func commandNames() []string {
	var names []string
	for _, c := range commands() {
		if c.name != "serve" {
			names = append(names, c.name)
		}
	}
	return names
}

func completionScript(shell string) (string, error) {
	cmds := strings.Join(commandNames(), " ")
	wifi := strings.Join(wifiSubs, " ")
	switch shell {
	case "zsh":
		var b strings.Builder
		b.WriteString("#compdef adbmount\n_adbmount() {\n  local -a cmds\n  cmds=(\n")
		for _, c := range commands() {
			if c.name != "serve" {
				fmt.Fprintf(&b, "    '%s:%s'\n", c.name, strings.ReplaceAll(c.short, "'", ""))
			}
		}
		b.WriteString("  )\n  if (( CURRENT == 2 )); then\n    _describe 'command' cmds\n    return\n  fi\n  case $words[2] in\n")
		fmt.Fprintf(&b, "    wifi) (( CURRENT == 3 )) && compadd %s ;;\n", wifi)
		b.WriteString("    up) _arguments '--ro[mount read-only]' '--only[mount only one volume]:volume:(android sdcard)' '-s[device serial]:serial:' ;;\n")
		b.WriteString("    down|log) compadd android sdcard ;;\n")
		b.WriteString("    pull|push) _arguments '-n[dry run]' '-f[copy even if unchanged]' '-j[parallel transfers]:jobs:' '-s[device serial]:serial:' '*:path:_files' ;;\n")
		b.WriteString("    completion) compadd zsh bash fish ;;\n  esac\n}\n_adbmount \"$@\"\n")
		return b.String(), nil
	case "bash":
		return fmt.Sprintf(`_adbmount() {
  local cur=${COMP_WORDS[COMP_CWORD]}
  if [ "$COMP_CWORD" -eq 1 ]; then COMPREPLY=($(compgen -W "%s" -- "$cur")); return; fi
  case ${COMP_WORDS[1]} in
    wifi) [ "$COMP_CWORD" -eq 2 ] && COMPREPLY=($(compgen -W "%s" -- "$cur")) ;;
    up) COMPREPLY=($(compgen -W "--ro --only -s" -- "$cur")) ;;
    down|log) COMPREPLY=($(compgen -W "android sdcard" -- "$cur")) ;;
    pull|push) COMPREPLY=($(compgen -f -- "$cur")) ;;
    completion) COMPREPLY=($(compgen -W "zsh bash fish" -- "$cur")) ;;
  esac
}
complete -F _adbmount adbmount
`, cmds, wifi), nil
	case "fish":
		var b strings.Builder
		b.WriteString("complete -c adbmount -f\n")
		for _, c := range commands() {
			if c.name != "serve" {
				fmt.Fprintf(&b, "complete -c adbmount -n '__fish_use_subcommand' -a %s -d '%s'\n", c.name, strings.ReplaceAll(c.short, "'", ""))
			}
		}
		fmt.Fprintf(&b, "complete -c adbmount -n '__fish_seen_subcommand_from wifi' -a '%s'\n", wifi)
		b.WriteString("complete -c adbmount -n '__fish_seen_subcommand_from up' -l ro -d 'mount read-only'\n")
		b.WriteString("complete -c adbmount -n '__fish_seen_subcommand_from up' -l only -a 'android sdcard'\n")
		b.WriteString("complete -c adbmount -n '__fish_seen_subcommand_from down log' -a 'android sdcard'\n")
		b.WriteString("complete -c adbmount -n '__fish_seen_subcommand_from pull push' -F\n")
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown shell %q (zsh, bash or fish)", shell)
}

func runCompletion(args []string) error {
	if len(args) != 1 {
		return fail("usage: adbmount completion zsh|bash|fish\n  zsh:  adbmount completion zsh > \"${fpath[1]}/_adbmount\"\n  bash: source <(adbmount completion bash)")
	}
	s, err := completionScript(args[0])
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}
