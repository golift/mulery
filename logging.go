package mulery

import (
	"log"
	"os"
	"strings"

	"golift.io/rotatorr"
	"golift.io/rotatorr/timerotator"
)

// SetupLogs starts the logs rotation and sets logger output to the configured file(s).
// You must call this before calling Start to setup logs, or things will panic.
//
//nolint:mnd
func (c *Config) SetupLogs() {
	c.httpLog = log.New(os.Stdout, "", 0)

	if c.HTTPLog != "" && c.HTTPLogMB > 0 {
		c.httpLog.SetOutput(rotatorr.NewMust(&rotatorr.Config{
			Filepath: c.HTTPLog,
			FileSize: c.HTTPLogMB * 1024 * 1024, // 1 MB
			FileMode: 0o644,
			Rotatorr: &timerotator.Layout{FileCount: c.HTTPLogs},
		}))
	}

	if c.LogFile == "" {
		c.log = log.New(os.Stderr, "", log.LstdFlags)
		return
	}

	var rotator *rotatorr.Logger

	// This ensures panics write to the log file.
	postRotate := func(_, _ string) { os.Stderr = rotator.File }
	defer postRotate("", "")

	rotator = rotatorr.NewMust(&rotatorr.Config{
		Filepath: c.LogFile,
		FileSize: c.LogFileMB * 1024 * 1024,
		FileMode: 0o644,
		Rotatorr: &timerotator.Layout{
			FileCount:  c.LogFiles,
			PostRotate: postRotate,
		},
	})
	c.log = log.New(rotator, "", log.LstdFlags)
	log.SetOutput(rotator)

	if c.HTTPLog == "" || c.HTTPLogMB < 1 {
		c.httpLog.SetOutput(rotator)
	}
}

// Debugf writes log lines... to stdout and/or a file.
func (c *Config) Debugf(msg string, v ...interface{}) {
	c.log.Printf("[DEBUG] "+msg, v...)
}

// Printf writes log lines... to stdout and/or a file.
func (c *Config) Printf(msg string, v ...interface{}) {
	c.log.Printf("[INFO] "+msg, v...)
}

// Errorf writes log lines... to stdout and/or a file.
func (c *Config) Errorf(msg string, v ...interface{}) {
	c.log.Printf("[ERROR] "+msg, v...)
}

// PrintConfig logs the current configuration information.
func (c *Config) PrintConfig() {
	c.Printf("=> Mulery Starting, pid: %d", os.Getpid())
	c.Printf("=> Listen Address: %s", c.ListenAddr)
	c.Printf("=> Dispatch Threads: %d", c.Dispatchers)
	c.Printf("=> Auth URL/Header: %s / %s", c.AuthURL, c.AuthHeader)
	c.Printf("=> Allowed Requesters: %s", c.allow.String())
	c.Printf("=> CacheDir: %s", c.CacheDir)
	c.Printf("=> Email / Token: %s / %v", c.Email, len(c.CFToken) > 0)
	c.Printf("=> SSL Names: %s", strings.Join(c.SSLNames, ", "))
	c.Printf("=> Log File: %s (count: %d, size: %dMB)", c.LogFile, c.LogFiles, c.LogFileMB)
	c.Printf("=> HTTP Log: %s (count: %d, size: %dMB)", c.HTTPLog, c.HTTPLogs, c.HTTPLogMB)
	c.Printf("=> Log Format: %s", c.ApacheLogFormat())
}

//nolint:wsl
func (c *Config) ApacheLogFormat() string {
	if idh := c.IDHeader; len(c.LogHeaders) == 0 {
		if idh != "" {
			idh = `"%{` + c.IDHeader + `}i"`
		}
		return `%h - - %t "%r" %>s %b ` + idh + ` "%{User-agent}i" - %{ms}Tms`
	}

	apacheFormat := `%h `
	add := func(val string) {
		if val == "" {
			apacheFormat += "-"
		} else {
			apacheFormat += "%{" + val + "}i"
		}
	}

	add(c.LogHeaders["uid"])
	apacheFormat += " "
	add(c.LogHeaders["name"])
	apacheFormat += ` %t "%r" %>s %b "`
	add(c.IDHeader)
	apacheFormat += `" "%{User-agent}i" - %{ms}Tms`

	for name, val := range c.LogHeaders {
		if name != "uid" && name != "name" {
			apacheFormat += ` "` + name + `:%{` + val + `}i"`
		}
	}

	return apacheFormat
}
