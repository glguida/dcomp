package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glguida/dcomp/hostfs"
)

func runInit(args []string) int {
	var directory, group string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			fmt.Println("Usage: dcomp init DIR [--group GROUP]")
			return 0
		case arg == "--group":
			i++
			if i == len(args) {
				return commandUsage("--group requires GROUP")
			}
			group = args[i]
			if group == "" {
				return commandUsage("--group requires a nonempty group name")
			}
		case strings.HasPrefix(arg, "--group="):
			group = strings.TrimPrefix(arg, "--group=")
			if group == "" {
				return commandUsage("--group requires a nonempty group name")
			}
		case strings.HasPrefix(arg, "-"):
			return commandUsage("unknown init option: " + arg)
		default:
			if directory != "" {
				return commandUsage("init expects one directory")
			}
			directory = arg
		}
	}
	if directory == "" {
		return commandUsage("init expects DIR [--group GROUP]")
	}
	root, err := hostfs.Init(directory, group)
	if err == nil {
		for _, name := range []string{"systems", "run"} {
			if err = hostfs.MkdirAll(filepath.Join(root, name), 0700); err != nil {
				break
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("export DCOMP_STATE_ROOT='%s'\n", strings.ReplaceAll(root, "'", "'\\''"))
	return 0
}
