package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/BurntSushi/toml"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/mitchellh/go-homedir"
	"github.com/flynn/flynn/controller/client"
)

type Cluster struct {
	Name          string `json:"name"`
	Key           string `json:"key"`
	TLSPin        string `json:"tls_pin" toml:"TLSPin,omitempty"`
	ControllerURL string `json:"controller_url"`
	GitURL        string `json:"git_url"`

	// GitHost and URL are legacy config options for clusters that are using git
	// over SSH, they should be removed at some point in the near future.
	GitHost string `json:"git_host" toml:"GitHost,omitempty"`
	URL     string `json:"url" toml:"URL,omitempty"`

	// Domain was a short-lived option, replaced by ControllerURL and GitURL, it
	// should be removed along with the above.
	Domain string `json:"domain" toml:"Domain,omitempty"`
}

func (c *Cluster) Client() (*controller.Client, error) {
	var pin []byte
	if c.TLSPin != "" {
		var err error
		pin, err = base64.StdEncoding.DecodeString(c.TLSPin)
		if err != nil {
			return nil, fmt.Errorf("error decoding tls pin: %s", err)
		}
	}
	return controller.NewClientWithConfig(c.ControllerURL, c.Key, controller.Config{Pin: pin})
}

func (c *Cluster) SSHGit() bool {
	return c.GitHost != ""
}

type Config struct {
	Default  string     `toml:"default"`
	Clusters []*Cluster `toml:"cluster"`
}

func HomeDir() string {
	dir, err := homedir.Dir()
	if err != nil {
		panic(err)
	}
	return dir
}

func Dir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "flynn")
	}
	return filepath.Join(HomeDir(), ".flynn")
}

func DefaultPath() string {
	if p := os.Getenv("FLYNNRC"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(Dir(), "flynnrc")
	}
	return filepath.Join(HomeDir(), ".flynnrc")
}

func ReadFile(path string) (*Config, error) {
	c := &Config{}
	_, err := toml.DecodeFile(path, c)
	if err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) Marshal() []byte {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func (c *Config) Add(s *Cluster, force bool) error {
	var msg string
	conflictIdx := -1
	for i, existing := range c.Clusters {
		switch {
		case existing.Name == s.Name:
			msg = fmt.Sprintf("Cluster %q already exists in ~/.flynnrc", s.Name)
		case existing.GitURL != "" && existing.GitURL == s.GitURL:
			msg = fmt.Sprintf("A cluster with the URL %q already exists in ~/.flynnrc", s.GitURL)
		case existing.ControllerURL == s.ControllerURL:
			msg = fmt.Sprintf("A cluster with the URL %q already exists in ~/.flynnrc", s.ControllerURL)
		case existing.GitHost != "" && existing.GitHost == s.GitHost:
			msg = fmt.Sprintf("A cluster with the git host %q already exists in ~/.flynnrc", s.GitHost)
		}
		if msg != "" {
			if conflictIdx != -1 && conflictIdx != i {
				return fmt.Errorf("The cluster name and/or URLs conflict with multiple existing clusters.")
			}
			conflictIdx = i
		}
	}

	// The new cluster config conflicts with an existing one
	if msg != "" {
		if !force {
			return fmt.Errorf(msg)
		}

		// Remove conflicting cluster
		c.Clusters = append(c.Clusters[:conflictIdx], c.Clusters[conflictIdx+1:]...)
	}

	c.Clusters = append(c.Clusters, s)

	return nil
}

func (c *Config) Upgrade() (changed bool) {
	for _, cluster := range c.Clusters {
		if cluster.URL != "" {
			cluster.ControllerURL = cluster.URL
			cluster.URL = ""
			changed = true
		}
		if cluster.Domain != "" {
			cluster.ControllerURL = "https://controller." + cluster.Domain
			cluster.GitURL = "https://git." + cluster.Domain
			cluster.Domain = ""
			changed = true
		}
	}
	return changed
}

func (c *Config) Remove(name string) *Cluster {
	for i, s := range c.Clusters {
		if s.Name != name {
			continue
		}
		c.Clusters = append(c.Clusters[:i], c.Clusters[i+1:]...)
		return s
	}
	return nil
}

func (c *Config) SetDefault(name string) bool {
	for _, s := range c.Clusters {
		if s.Name != name {
			continue
		}
		c.Default = name
		return true
	}
	return false
}

func (c *Config) SaveTo(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(c.Clusters) != 0 {
		if err := toml.NewEncoder(f).Encode(c); err != nil {
			return err
		}
		f.Write([]byte("\n"))
	}
	return nil
}
