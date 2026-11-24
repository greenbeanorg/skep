package startcheck

import "path/filepath"

// ProgramTargets are the agent's binary and the configuration file it is
// about to read; configPath is empty when it reads none. The agent and
// skep-setup check share this list, so they cannot disagree.
func ProgramTargets(exe, configPath string) []Target {
	out := []Target{{Path: exe, What: "the agent's binary"}}
	if configPath != "" {
		out = append(out, Target{Path: configPath, What: "the agent's configuration"})
	}
	return out
}

// StateTargets are the data directory, the directory holding the database
// when it is another one, and the socket's directory. Any may not exist
// yet; the parents that do are checked.
func StateTargets(dataDir, dbPath, socketPath string) []Target {
	out := []Target{{Path: dataDir, What: "the agent's data directory", Optional: true}}
	if dir := filepath.Dir(dbPath); dir != filepath.Clean(dataDir) {
		out = append(out, Target{Path: dir, What: "the agent's database directory", Optional: true})
	}
	return append(out, Target{Path: filepath.Dir(socketPath), What: "the agent socket's directory", Optional: true})
}
