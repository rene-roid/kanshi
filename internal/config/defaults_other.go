//go:build !windows

package config

const defaultDockerHost = "unix:///var/run/docker.sock"
