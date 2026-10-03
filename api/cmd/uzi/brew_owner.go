package main

import "path/filepath"

// brewOwnerFromExecutable resolves the installed binary's Homebrew path without
// forking brew. Wrappers, broken links and unfamiliar layouts fall back to unknown.
func brewOwnerFromExecutable(executable func() (string, error)) string {
	if executable == nil {
		return ""
	}
	exe, err := executable()
	if err != nil || !filepath.IsAbs(exe) {
		return ""
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil || filepath.Base(exe) != "uzi" || filepath.Base(filepath.Dir(exe)) != "bin" {
		return ""
	}
	prefix := filepath.Dir(filepath.Dir(exe))
	formula := prefix
	if filepath.Base(filepath.Dir(prefix)) != "opt" {
		// Cellar/<formula>/<version>/bin/uzi, including opt/bin symlinks
		// resolved to the real keg above.
		formula = filepath.Dir(prefix)
		if filepath.Base(filepath.Dir(formula)) != "Cellar" {
			return ""
		}
	}
	switch owner := filepath.Base(formula); owner {
	case "uzi-cli", "uzi-cli-rc":
		return owner
	default:
		return ""
	}
}
