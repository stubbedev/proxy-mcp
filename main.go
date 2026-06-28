package main

import (
	"fmt"
	"log"

	flag "github.com/spf13/pflag"
)

var BuildVersion = "dev"

func main() {
	// GNU-style flags: every flag has a `--long` form and most a `-short`
	// alias. pflag provides `-h`/`--help` automatically.
	conf := flag.StringP("config", "c", "config.json", "path to config file or a http(s) url")
	insecure := flag.BoolP("insecure", "k", false, "allow insecure HTTPS connections by skipping TLS certificate verification")
	expandEnv := flag.BoolP("expand-env", "e", true, "expand environment variables in the config file")
	httpHeaders := flag.StringP("http-headers", "H", "", "HTTP headers for the config URL, format: 'Key1:Value1;Key2:Value2'")
	httpTimeout := flag.IntP("http-timeout", "t", 10, "HTTP timeout in seconds when fetching config from a URL")
	validate := flag.BoolP("validate", "V", false, "load and validate the config, then exit (0 ok, 1 invalid) without starting the server")
	idleTimeout := flag.DurationP("idle-timeout", "i", 0, "exit after this much idle time with no proxied requests (e.g. 5m); 0 disables")
	version := flag.BoolP("version", "v", false, "print version and exit")
	flag.Parse()

	if *version {
		fmt.Println(BuildVersion)
		return
	}
	// loadConfig re-reads the config with the same flags, so a reload (SIGHUP or
	// config-file change) picks up edits without a restart.
	loadConfig := func() (*Config, error) {
		return load(*conf, *insecure, *expandEnv, *httpHeaders, *httpTimeout)
	}
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if *validate {
		if vErr := validateConfig(config); vErr != nil {
			log.Fatalf("Config invalid: %v", vErr)
		}
		fmt.Printf("config ok: %d server(s)\n", len(config.McpServers))
		return
	}
	err = startHTTPServer(config, *idleTimeout, loadConfig, *conf)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
