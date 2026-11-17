package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/NanQiaoMi/auto-harness/plugins"
)

type pluginCommandOptions struct {
	Index         string
	Install       string
	Remove        string
	List          bool
	Verify        bool
	Trust         string
	Untrust       string
	TrustList     bool
	AllowUnsigned bool
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
		strings.TrimSpace(options.Trust) != "",
		strings.TrimSpace(options.Untrust) != "",
		options.TrustList,
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
		store, err := plugins.LoadPublisherStore(plugins.PublisherStorePath())
		if err != nil {
			return true, 1, err
		}
		requireSignature := plugins.IsRemoteRegistry(index) && !options.AllowUnsigned
		policy, err := store.Policy(requireSignature)
		if err != nil {
			return true, 1, err
		}
		catalog, err := plugins.OpenCatalogWithPolicy(index, policy)
		if err != nil {
			return true, 1, err
		}
		if plugins.IsRemoteRegistry(index) && !catalog.Verified {
			fmt.Fprintln(os.Stderr,
				"warning: installing from an unsigned remote registry because -plugin-allow-unsigned is set")
		}
		records, err := plugins.Install(root, catalog, name, constraint)
		if err != nil {
			return true, 1, err
		}
		if catalog.Verified {
			fmt.Printf("verified publisher %s (%s)\n", catalog.Publisher, catalog.SignerKeyID)
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
			publisher := item.Publisher
			if publisher == "" {
				publisher = "unsigned"
			}
			fmt.Printf("%-20s %-10s %-16s %-8s %s\n",
				item.Name, item.Version, publisher, state, root)
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
	case strings.TrimSpace(options.Trust) != "":
		publisher, keyPath, err := splitPublisherKeySpec(options.Trust)
		if err != nil {
			return true, 1, err
		}
		key, err := plugins.ReadPublicKeyFile(keyPath)
		if err != nil {
			return true, 1, err
		}
		store, err := plugins.LoadPublisherStore(plugins.PublisherStorePath())
		if err != nil {
			return true, 1, err
		}
		keyID, err := store.Add(publisher, key)
		if err != nil {
			return true, 1, err
		}
		if err := store.Write(plugins.PublisherStorePath()); err != nil {
			return true, 1, err
		}
		fmt.Printf("trusted publisher %s with %s\n", publisher, keyID)
	case strings.TrimSpace(options.Untrust) != "":
		publisher := strings.TrimSpace(options.Untrust)
		store, err := plugins.LoadPublisherStore(plugins.PublisherStorePath())
		if err != nil {
			return true, 1, err
		}
		if err := store.RemovePublisher(publisher); err != nil {
			return true, 1, err
		}
		if err := store.Write(plugins.PublisherStorePath()); err != nil {
			return true, 1, err
		}
		fmt.Printf("removed trust for publisher %s\n", publisher)
	case options.TrustList:
		store, err := plugins.LoadPublisherStore(plugins.PublisherStorePath())
		if err != nil {
			return true, 1, err
		}
		entries, err := store.Entries()
		if err != nil {
			return true, 1, err
		}
		if len(entries) == 0 {
			fmt.Println("no trusted plugin publishers")
			return true, 0, nil
		}
		for _, entry := range entries {
			fmt.Println(entry.Publisher)
			for _, keyID := range entry.KeyIDs {
				fmt.Printf("  %s\n", keyID)
			}
		}
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

func splitPublisherKeySpec(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	publisher, keyPath, ok := strings.Cut(value, "=")
	publisher = strings.TrimSpace(publisher)
	keyPath = strings.TrimSpace(keyPath)
	if !ok || publisher == "" || keyPath == "" {
		return "", "", fmt.Errorf("trusted publisher must be publisher=/path/to/public-key")
	}
	return publisher, keyPath, nil
}
