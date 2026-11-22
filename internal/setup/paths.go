package setup

import (
	"os"

	"github.com/greenbeanorg/skep/internal/config"
)

// noEnv is the environment setup reads a service's configuration with:
// none. The rc.d scripts start the services through service(8), which
// clears the environment too.
func noEnv(string) (string, bool) { return "", false }

// serviceConfigFile returns the configuration file an rc.d script passes
// as -config (rcVar, for example skep_agent_config) and the data
// directory it passes as -data-dir (dataVar). An empty file means the
// program's default, read when it exists.
func serviceConfigFile(root, rcVar, dataVar string) (file, dataDir string, err error) {
	rc, err := ReadRCConf(root)
	if err != nil {
		return "", "", err
	}
	file, _ = rc.Get(rcVar)
	dataDir, _ = rc.Get(dataVar)
	if dataDir == "" {
		dataDir = config.DefaultDataDir // the rc.d scripts' default
	}
	return file, dataDir, nil
}

// AgentDBPath returns the host path (under root) of the agent's database
// as the running agent has it: agent.json (or skep_agent_config) loaded
// the way the agent loads it, with the rc.d script's -data-dir.
func AgentDBPath(root string) (string, error) {
	file, dataDir, err := serviceConfigFile(root, "skep_agent_config", "skep_agent_data_dir")
	if err != nil {
		return "", err
	}
	opts := config.LoadOptions{DefaultPath: hostPath(root, config.DefaultConfigPath), LookupEnv: noEnv,
		Overrides: config.Overrides{DataDir: dataDir}}
	if file != "" {
		opts.Path = hostPath(root, file)
	}
	c, err := config.Load(opts)
	if err != nil {
		return "", err
	}
	return hostPath(root, c.Database.Path), nil
}

// APIDBPath returns the host path (under root) of the API's database, from
// api.json (or skep_api_config) and the rc.d script's -data-dir.
func APIDBPath(root string) (string, error) {
	file, dataDir, err := serviceConfigFile(root, "skep_api_config", "skep_api_data_dir")
	if err != nil {
		return "", err
	}
	opts := config.APILoadOptions{DefaultPath: hostPath(root, config.DefaultAPIConfigPath), LookupEnv: noEnv,
		Overrides: config.APIOverrides{DataDir: dataDir}}
	if file != "" {
		opts.Path = hostPath(root, file)
	}
	c, err := config.LoadAPI(opts)
	if err != nil {
		return "", err
	}
	return hostPath(root, c.Database.Path), nil
}

// exists reports whether path exists (any type), without following a final
// symbolic link.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// AgentStorage returns the agent's allow-listed pools and dataset root as
// the running agent has them: agent.json (or skep_agent_config) loaded the
// way the agent loads it. Without a configuration file the agent uses the
// defaults: no pools.
func AgentStorage(root string) (pools []string, datasetRoot string, err error) {
	file, dataDir, err := serviceConfigFile(root, "skep_agent_config", "skep_agent_data_dir")
	if err != nil {
		return nil, "", err
	}
	opts := config.LoadOptions{DefaultPath: hostPath(root, config.DefaultConfigPath), LookupEnv: noEnv,
		Overrides: config.Overrides{DataDir: dataDir}}
	if file != "" {
		opts.Path = hostPath(root, file)
	}
	c, err := config.Load(opts)
	if err != nil {
		return nil, "", err
	}
	return c.Storage.Pools, c.Storage.DatasetRoot, nil
}
