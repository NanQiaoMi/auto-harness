package main

import (
	"fmt"
	"strings"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

type pluginCommandOptions struct {
	Index   string
	Install string
	Remove  string
	List    bool
	Verify  bool
}

// runPluginCommand handles the standalone plugin-management actions
// before a model, session or extension is assembled.
func runPluginCommand(options pluginCommandOptions) (bool, int, error) {
	actions := 0
	for _, active := range []bool{
		strings.TrimSpace(options.Install) != "",
		strings.TrimSpace(options.Remove) != "",
		options.List,
		options.Verify,
	} {
		if active {
			actions++
		}
	}
	if actions == 0 {
		return false, 0, nil
	}
	if actions > 1 {
		return true, 1, fmt.Errorf("choose only one plugin action")
	}
	root := plugins.InstallDir()
	switch {
	case strings.TrimSpace(options.Install) != "":
		index := strings.TrimSpace(options.Index)
		if index == "" {
			return true, 1, fmt.Errorf("-plugin-install requires -plugin-index or HARNESS_PLUGIN_INDEX")
		}
		name, constraint, err := splitPluginSpec(options.Install)
		if err != nil {
			return true, 1, err
		}
		catalog, err := plugins.OpenCatalog(index)
		if err != nil {
			return true, 1, err
		}
		records, err := plugins.Install(root, catalog, name, constraint)
		if err != nil {
			return true, 1, err
		}
		for _, record := range records {
			fmt.Printf("installed %s %s sha256:%s\n", record.Name, record.Version, record.Digest)
		}
	case strings.TrimSpace(options.Remove) != "":
		name := strings.TrimSpace(options.Remove)
		if err := plugins.Remove(root, name); err != nil {
			return true, 1, err
		}
		fmt.Printf("removed %s\n", name)
	case options.List:
		installed, err := plugins.ListInstalled(root)
		if err != nil {
			return true, 1, err
		}
		if len(installed) == 0 {
			fmt.Println("no managed plugins")
			return true, 0, nil
		}
		for _, item := range installed {
			state := "ok"
			if !item.Healthy {
				state = "damaged: " + item.Problem
			}
			fmt.Printf("%-20s %-10s %-8s %s\n", item.Name, item.Version, state, root)
		}
	case options.Verify:
		issues, err := plugins.Verify(root)
		if err != nil {
			return true, 1, err
		}
		for _, issue := range issues {
			fmt.Println(issue)
		}
		if len(issues) > 0 {
			return true, 1, nil
		}
		fmt.Println("plugin lock and installed trees verified")
	}
	return true, 0, nil
}

func splitPluginSpec(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	name, constraint, ok := strings.Cut(value, "@")
	if !ok {
		if name == "" {
			return "", "", fmt.Errorf("plugin name is empty")
		}
		return name, "*", nil
	}
	name = strings.TrimSpace(name)
	constraint = strings.TrimSpace(constraint)
	if name == "" || constraint == "" {
		return "", "", fmt.Errorf("plugin spec must be name or name@version-constraint")
	}
	return name, constraint, nil
}
