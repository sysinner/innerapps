// Copyright 2015 Eryx <evorui at gmail dot com>, All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sysinner/innerstack/v2/pkg/inconf"
)

var (
	pgVer    = "18"
	pgVers   = []string{"18"}
	pgMemMin = int32(16) // in MiB
	mu       sync.Mutex
	cfgMu    sync.Mutex
	cfgLast  EnvConfig
	cfgNext  EnvConfig
	appCfr   inconf.AppReplicaHelper
)

func pgKeeperPath(path string) string {
	return filepath.Clean("/opt/postgresql/keeper/" + path)
}

func pgVerPath(path string) string {
	return filepath.Clean("/opt/postgresql/postgresql" + pgVer + "/" + path)
}

func pgPrefix() string {
	return "/opt/postgresql/postgresql" + pgVer
}

// pgSocketDir is the unix-socket directory postgres listens on (configured as
// unix_socket_directories in postgresql.conf) and the directory every client
// tool (createuser/createdb/psql) connects through via -h. The two MUST agree:
// the tools open "<pgSocketDir>/.s.PGSQL.5432", so a mismatch surfaces as
// "connection to server on socket ... failed: No such file or directory".
// /tmp is used rather than the data dir so the path already matches the
// running server without forcing a restart, since unix_socket_directories is
// not reloadable.
const pgSocketDir = "/tmp"

type EnvConfig struct {
	Inited    bool                `json:"inited"`
	RootAuth  string              `json:"root_auth"`
	Resource  EnvConfigResource   `json:"resource"`
	Database  EnvConfigDatabase   `json:"database"`
	Databases []EnvConfigDatabase `json:"databases,omitempty"`
	Users     []EnvConfigUser     `json:"users"`
	Updated   int64               `json:"updated"`
	flush     bool
}

type EnvConfigResource struct {
	Mem int64 `json:"mem"`
	Cpu int64 `json:"cpu"`
}

type EnvConfigDatabase struct {
	Name string `json:"name"`
	// Owner is the postgres role that owns this database. For the main
	// database it is left empty (handled by the legacy db_user flow); for
	// additional databases from the "databases" array_group it carries the
	// per-instance db_user.
	Owner string `json:"owner,omitempty"`
	// Auth is the password for the Owner role (additional databases only).
	Auth string `json:"auth,omitempty"`
}

type EnvConfigUser struct {
	Name string `json:"name"`
	Auth string `json:"auth"`
}

func (cfg *EnvConfig) UserSync(item EnvConfigUser) {

	cfgMu.Lock()
	defer cfgMu.Unlock()

	for i, v := range cfg.Users {

		if v.Name == item.Name {
			cfg.Users[i] = item
			return
		}
	}

	cfg.Users = append(cfg.Users, item)
}

func (cfg *EnvConfig) UserGet(name string) *EnvConfigUser {

	cfgMu.Lock()
	defer cfgMu.Unlock()

	for _, v := range cfg.Users {

		if v.Name == name {
			return &v
		}
	}

	return nil
}

// DatabaseSync upserts an additional database entry by name.
func (cfg *EnvConfig) DatabaseSync(item EnvConfigDatabase) {

	cfgMu.Lock()
	defer cfgMu.Unlock()

	for i, v := range cfg.Databases {
		if v.Name == item.Name {
			cfg.Databases[i] = item
			return
		}
	}

	cfg.Databases = append(cfg.Databases, item)
}

// DatabaseGet returns the additional database entry for name, or nil.
func (cfg *EnvConfig) DatabaseGet(name string) *EnvConfigDatabase {

	cfgMu.Lock()
	defer cfgMu.Unlock()

	for _, v := range cfg.Databases {
		if v.Name == name {
			return &v
		}
	}

	return nil
}

func main() {

	{
		if err := os.MkdirAll(pgKeeperPath("var/log"), 0755); err != nil {
			panic(err)
		}
		fp, err := os.OpenFile(
			pgKeeperPath("var/log/pgkeeper.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND,
			0666,
		)
		if err != nil {
			panic(err)
		}
		defer fp.Close()
	}

	pgVerArg := flag.String("major_version", "", "postgresql major version")
	flag.Parse()

	if slices.Contains(pgVers, *pgVerArg) {
		pgVer = *pgVerArg
	} else {
		panic("invalid --major_version")
	}

	do()
}

func do() {

	_, err := os.Stat("/usr/bin/postgres")
	if err != nil {
		slog.Error(err.Error())
		return
	}

	var (
		tstart = time.Now()
	)

	cfgNext = EnvConfig{}

	//
	if appCfr != nil && !appCfr.Update() {
		return
	}

	if appCfr, err = inconf.NewAppReplicaHelper(); err != nil {
		slog.Error(err.Error())
		return
	}
	// slog.Info("load app-instance.json", "app", appCfr)

	slog.Info("load config", "cfg", appCfr.ReplicaInstance().App.Deploy.Configs)

	// MemoryLimit is in bytes, convert to MiB
	if appCfr.ReplicaInstance().App.Deploy.MemoryLimit > 0 {
		cfgNext.Resource.Mem = appCfr.ReplicaInstance().App.Deploy.MemoryLimit / (1 << 20)
	}
	// CpuLimit is in milli-cores (1 CPU = 1000 milli-cores)
	if appCfr.ReplicaInstance().App.Deploy.CpuLimit > 0 {
		cfgNext.Resource.Cpu = appCfr.ReplicaInstance().App.Deploy.CpuLimit
	}

	if item := appCfr.ConfigItem("db_name"); item != nil {
		cfgNext.Database = EnvConfigDatabase{
			Name: item.Value,
		}
	} else {
		slog.Error("No db_name Found")
		return
	}

	if item1 := appCfr.ConfigItem("db_user"); item1 != nil {

		item2 := appCfr.ConfigItem("db_auth")
		if item2 == nil {
			slog.Error("No db_auth Found")
			return
		}

		cfgNext.UserSync(EnvConfigUser{
			Name: item1.Value,
			Auth: item2.Value,
		})

	} else {
		slog.Error("No db_user Found")
		return
	}

	// Additional databases declared via the "databases" array_group config.
	// Each replicated group instance carries its own db_name (the key_item),
	// db_user and db_auth; they are provisioned alongside the main database.
	// The deploy-time validator (unique="app") already guarantees db_user
	// values are distinct from the main db_user and from each other.
	if ag := appCfr.ConfigItem("databases"); ag != nil {
		for _, inst := range ag.Items {
			if inst == nil {
				continue
			}
			var db EnvConfigDatabase
			for _, f := range inst.Items {
				if f == nil {
					continue
				}
				switch f.Name {
				case "db_name":
					db.Name = f.Value
				case "db_user":
					db.Owner = f.Value
				case "db_auth":
					db.Auth = f.Value
				}
			}
			// skip entries without a key, or that duplicate the main database.
			if db.Name == "" || db.Name == cfgNext.Database.Name {
				continue
			}
			cfgNext.Databases = append(cfgNext.Databases, db)
		}
	}

	if item := appCfr.ConfigItem("memory_usage_limit"); item != nil {

		ramPc64, _ := strconv.ParseInt(item.Value, 10, 64)

		if ramPc64 < 10 || ramPc64 > 100 {
			slog.Error("Invalid memory_usage_limit Setup")
			return
		}

		ramPc := (cfgNext.Resource.Mem * ramPc64) / 100
		if offset := ramPc % int64(pgMemMin); offset > 0 {
			ramPc -= offset
		}
		if ramPc < int64(pgMemMin) {
			ramPc = int64(pgMemMin)
		}
		if ramPc < cfgNext.Resource.Mem {
			cfgNext.Resource.Mem = ramPc
		}

	} else {
		slog.Error("No memory_usage_limit Found")
		return
	}

	//
	if cfgNext.Resource.Mem < int64(pgMemMin) {
		slog.Error("Not enough Memory to fit this PostgreSQL Instance")
		return
	}

	//
	if cfgLast.Database.Name == "" {
		if err := jsonDecodeFromFile(
			pgVerPath("init_option.json"),
			&cfgLast,
		); err != nil &&
			!os.IsNotExist(err) {
			slog.Error("load init_option.json err " + err.Error())
			return
		}
	}

	// s1
	if err := initDataDir(); err != nil {
		slog.Error(err.Error())
		return
	}

	//
	if err := initConf(); err != nil {
		slog.Error(err.Error())
		return
	}

	if cfgLast.Resource.Mem != cfgNext.Resource.Mem ||
		cfgLast.Resource.Cpu != cfgNext.Resource.Cpu {
		if err := restart(); err != nil {
			slog.Error(err.Error())
			return
		}

		cfgLast.Resource.Mem = cfgNext.Resource.Mem
		cfgLast.Resource.Cpu = cfgNext.Resource.Cpu

		if err := jsonEncodeToFileIndent(pgVerPath("init_option.json"), cfgLast, 0644); err != nil {
			return
		}
	} else {

		if err := start(); err != nil {
			slog.Error(err.Error())
			return
		}
	}

	// s3
	if err := initDB(); err != nil {
		slog.Error("init_db", "err", err.Error())
		return
	}

	if err := initUser(); err != nil {
		slog.Error("init_user", "err", err.Error())
		return
	}

	if err := initDatabases(); err != nil {
		slog.Error("init_databases", "err", err.Error())
		return
	}

	slog.Info("setup complete", "duration", time.Since(tstart))
}

func initConf() error {

	if cfgLast.Inited &&
		cfgLast.Resource.Mem == cfgNext.Resource.Mem &&
		cfgLast.Resource.Cpu == cfgNext.Resource.Cpu {
		return nil
	}

	//
	// mem := int(cfgNext.Resource.Mem)
	// sets := map[string]interface{}{
	// 	"project_prefix": pgPrefix(),
	// 	"env_ram_size":   fmt.Sprintf("%dM", mem),
	// 	// "server_key_buffer_size":         fmt.Sprintf("%dM", mem/4),
	// 	// "server_query_cache_size":        fmt.Sprintf("%dM", mem/8),
	// 	// "server_innodb_buffer_pool_size": fmt.Sprintf("%dM", mem/4),
	// }

	if mgr, err := NewConfigFileManager(pgVerPath("data/postgresql.conf")); err == nil {
		mgr.Update(map[string]string{
			"unix_socket_directories": pgSocketDir,
			"listen_addresses":        "*",
			"port":                    "5432",
			"password_encryption":     "scram-sha-256",
		})
		if err = mgr.Flush(); err != nil {
			return err
		}
	}

	if mgr, err := NewHbaFileManager(pgVerPath("data/pg_hba.conf")); err == nil {
		mgr.Update(HbaRecord{
			Type:     "host",
			Database: "all",
			User:     "all",
			Address:  "0.0.0.0/0",
		}, "scram-sha-256")
		if err = mgr.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func initDataDir() error {

	mu.Lock()
	defer mu.Unlock()

	if cfgLast.Inited {
		return nil
	}

	if cfgLast.RootAuth != "" {
		return errors.New("Root Password Already Setup")
	}

	if err := os.MkdirAll(pgVerPath("data"), 0755); err != nil {
		return err
	}

	// writeable test!
	cfgLast.Updated = time.Now().UnixMilli()
	if err := jsonEncodeToFileIndent(pgVerPath("init_option.json"), cfgLast, 0644); err != nil {
		return err
	}

	_, err := os.Stat(pgVerPath("data/postgresql.conf"))
	if err != nil && os.IsNotExist(err) {
		_, err = runCmd(120*time.Second, "/usr/bin/initdb", "-D", pgVerPath("data"))
		if err != nil {
			slog.Error("initdb", "err", err.Error())
		} else {
			slog.Info("initdb ok")
		}
	}

	if err == nil {
		cfgLast.Inited = true
		err = jsonEncodeToFileIndent(pgVerPath("init_option.json"), cfgLast, 0644)
	}

	return err
}

func cleanRunlock() {
	os.Remove(pgVerPath("postmaster.pid"))
	os.Remove(pgVerPath("data/postmaster.pid"))
}

func start() error {

	mu.Lock()
	defer mu.Unlock()

	if !cfgLast.Inited {
		return errors.New("No Init")
	}

	if pidof() > 0 {
		return nil
	}

	cleanRunlock()
	os.Chmod(pgVerPath("data"), 0700)
	out, err := runCmd(90*time.Second, "/usr/bin/pg_ctl",
		"-D", pgVerPath("data"),
		"-l", pgVerPath("server_logfile.log"), "start")

	time.Sleep(1e9)

	if err != nil {
		slog.Error("start postgres", "err", err.Error(), "output", string(out))
	} else {
		slog.Info("start postgres ok")
	}

	return err
}

func restart() error {

	mu.Lock()
	defer mu.Unlock()

	if !cfgLast.Inited {
		return errors.New("No Init")
	}

	var err error

	if pid := pidof(); pid > 0 {
		slog.Info("reloa", "pid", pid)
		_, err = runCmd(30*time.Second, "/usr/bin/pg_ctl",
			"-D", pgVerPath("data"),
			"-l", pgVerPath("server_logfile.log"), "reload")
		if err != nil {
			slog.Error("reload err " + err.Error())
		} else {
			slog.Info("reload ok")
		}
	} else {
		cleanRunlock()
		os.Chmod(pgVerPath("data"), 0700)
		_, err = runCmd(90*time.Second, "/usr/bin/pg_ctl",
			"-D", pgVerPath("data"),
			"-l", pgVerPath("server_logfile.log"), "start")
		time.Sleep(1e9)
		if err != nil {
			slog.Error("start postgres", "err", err.Error())
		} else {
			slog.Info("start postgres ok")
		}
	}

	return err
}

func pidof() int {

	//
	for i := 0; i < 3; i++ {

		output, err := runCmd(10*time.Second, "/usr/bin/pg_ctl",
			"-D", pgVerPath("data"), "status")
		if err == nil {
			re := regexp.MustCompile(`PID: (\[?(\d+)\]?)`)
			match := re.FindStringSubmatch(string(output))

			if len(match) > 2 {
				pid, err := strconv.Atoi(match[2])
				if err == nil {
					return pid
				}
			}
		}

		time.Sleep(3e9)
	}

	return 0
}

func initDB() error {

	mu.Lock()
	defer mu.Unlock()

	if !cfgLast.Inited {
		return errors.New("No Init")
	}

	var err error

	if cfgNext.Database.Name != "" &&
		cfgLast.Database.Name == "" {

		runCmd(15*time.Second, "/usr/bin/createuser", "-h", pgSocketDir, "-D", "dbuser")

		_, err = runCmd(15*time.Second, "/usr/bin/createdb",
			"-h", pgSocketDir,
			"--owner=dbuser", cfgNext.Database.Name)
		if err != nil {
			slog.Error("initdb", "err", err.Error())
			return err
		} else {
			slog.Info("initdb ok")
		}

		slog.Info("create database ok", "name", cfgNext.Database.Name)

		cfgLast.Database = cfgNext.Database
		err = jsonEncodeToFileIndent(pgVerPath("init_option.json"), cfgLast, 0644)
	}

	return err
}

func initUser() error {

	if !cfgLast.Inited {
		return errors.New("No Init")
	}

	if cfgLast.Database.Name == "" {
		return errors.New("No Database Found")
	}

	for _, v := range cfgNext.Users {

		if prev := cfgLast.UserGet(v.Name); prev == nil || prev.Auth != v.Auth {

			if err := pgSetPassword(v.Name, v.Auth, cfgLast.Database.Name); err != nil {
				return err
			}

			cfgLast.UserSync(v)
			if err := jsonEncodeToFileIndent(
				pgVerPath("init_option.json"),
				cfgLast,
				0644,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

// initDatabases provisions the additional databases declared in the "databases"
// array_group config. For each entry it ensures the owner role exists, creates
// the database (owned by that role), and sets the owner password. The whole
// step is idempotent: an entry already recorded in cfgLast with an unchanged
// owner/auth is skipped, and "already exists" errors from createuser/createdb
// are tolerated so partial reruns converge.
func initDatabases() error {

	mu.Lock()
	defer mu.Unlock()

	if !cfgLast.Inited {
		return errors.New("No Init")
	}

	for _, db := range cfgNext.Databases {

		if db.Name == "" {
			continue
		}

		// skip if already provisioned and unchanged
		if prev := cfgLast.DatabaseGet(db.Name); prev != nil &&
			prev.Owner == db.Owner && prev.Auth == db.Auth {
			continue
		}

		// ensure the owner role exists (tolerate "role already exists")
		if db.Owner != "" {
			if out, err := runCmd(15*time.Second, "/usr/bin/createuser",
				"-h", pgSocketDir, db.Owner); err != nil {
				if !pgAlreadyExists(out) {
					slog.Error("createuser", "name", db.Owner,
						"err", err.Error(), "output", string(out))
					return err
				}
			}
		}

		// create the database, owned by the role if declared
		args := []string{"-h", pgSocketDir}
		if db.Owner != "" {
			args = append(args, "--owner="+db.Owner)
		}
		args = append(args, db.Name)
		if out, err := runCmd(15*time.Second, "/usr/bin/createdb", args...); err != nil {
			if !pgAlreadyExists(out) {
				slog.Error("createdb", "name", db.Name,
					"err", err.Error(), "output", string(out))
				return err
			}
		}
		slog.Info("create database ok", "name", db.Name, "owner", db.Owner)

		// set / update the owner password
		if db.Owner != "" && db.Auth != "" {
			if err := pgSetPassword(db.Owner, db.Auth, db.Name); err != nil {
				return err
			}
		}

		cfgLast.DatabaseSync(db)
		if err := jsonEncodeToFileIndent(pgVerPath("init_option.json"), cfgLast, 0644); err != nil {
			return err
		}
	}

	return nil
}

// pgSetPassword sets/updates a postgres role's password by running an ALTER
// USER statement through psql over the local unix socket, bounded by a timeout
// so a wedged psql cannot pin the keeper.
func pgSetPassword(user, auth, dbname string) error {
	stmt := fmt.Sprintf("ALTER USER %s PASSWORD '%s'", user, auth)
	out, err := runCmd(15*time.Second, "/usr/bin/psql",
		"-h", pgSocketDir, "-U", user, "-d", dbname, "-c", stmt)
	if err != nil {
		slog.Error("set user password", "user", user,
			"err", err.Error(), "output", string(out))
		return err
	}
	slog.Info("set user password", "user", user)
	return nil
}

// pgAlreadyExists reports whether postgres tooling output indicates the target
// object already exists, so a benign re-run does not fail the whole step.
func pgAlreadyExists(out []byte) bool {
	return strings.Contains(string(out), "already exists")
}

// runCmd runs name with args, returning combined stdout+stderr, bounded by
// timeout. Every postgres client/server tool call goes through here so a wedged
// child (a client blocked on an unresponsive socket, or pg_ctl waiting on a
// server that never comes up) can never pin the keeper and stall the keepalive
// task. On timeout the child is killed and the error is context.DeadlineExceeded.
//
// WaitDelay is the second safeguard: pg_ctl start spawns postgres as a
// long-lived grandchild that inherits and keeps open the stdout/stderr pipes,
// which would otherwise block CombinedOutput forever even after pg_ctl exits
// (CommandContext only kills the direct child). WaitDelay force-closes those
// pipes shortly after the child exits, bounding the wait.
func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

// ConfigFileManager manages PostgreSQL configuration file content,
// providing methods to read, modify, and persist changes with backup support.
type ConfigFileManager struct {
	FilePath   string // path to the configuration file
	RawContent string // original content for backup
	Content    string // modified content pending flush
}

// NewConfigFileManager reads the specified file and returns a manager instance.
func NewConfigFileManager(file string) (*ConfigFileManager, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	return &ConfigFileManager{
		FilePath:   file,
		RawContent: string(content),
		Content:    string(content),
	}, nil
}

// Update sets multiple key-value pairs in the configuration.
// PostgreSQL uses the last occurrence as effective value, so this method
// updates the last matching line (active or commented) or appends if not found.
func (m *ConfigFileManager) Update(values map[string]string) {
	lines := strings.Split(m.Content, "\n")

	for key, value := range values {
		// compile regex patterns for matching config lines
		// activeRe matches active config lines: optional leading spaces, key, equals sign
		activeRe := regexp.MustCompile(fmt.Sprintf(`(?i)^\s*%s\s*=`, regexp.QuoteMeta(key)))
		// commentRe matches commented defaults: optional leading spaces, #, key, equals sign
		commentRe := regexp.MustCompile(fmt.Sprintf(`(?i)^\s*#\s*%s\s*=`, regexp.QuoteMeta(key)))

		lastActiveIdx := -1
		lastCommentIdx := -1

		// scan to find the last occurrence of each pattern
		// PostgreSQL treats the last occurrence as the effective value
		for i, line := range lines {
			if activeRe.MatchString(line) {
				lastActiveIdx = i
			} else if commentRe.MatchString(line) {
				lastCommentIdx = i
			}
		}

		// format new config line with quoted value
		newLine := fmt.Sprintf("%s = '%s'", key, value)

		// apply update based on found location
		if lastActiveIdx != -1 {
			// case A: replace existing active config line
			lines[lastActiveIdx] = newLine
		} else if lastCommentIdx != -1 {
			// case B: replace commented default to activate it
			lines[lastCommentIdx] = newLine
		} else {
			// case C: append new config at end of file
			lines = append(lines, newLine)
		}
	}

	m.Content = strings.Join(lines, "\n")
}

// Flush writes modified content to disk with backup.
// Creates a timestamped backup of original content before overwriting.
// Returns nil if no changes detected.
func (m *ConfigFileManager) Flush() error {
	if m.RawContent == m.Content {
		return nil
	}
	backPath := fmt.Sprintf("%s_%s", m.FilePath, time.Now().Format("20060102.150405"))
	if err := os.WriteFile(backPath, []byte(m.RawContent), 0640); err != nil {
		return err
	}
	return os.WriteFile(m.FilePath, []byte(m.Content), 0640)
}

// HbaRecord represents a single line in pg_hba.conf file.
type HbaRecord struct {
	Type     string // local, host, hostssl, hostgssenc, hostnogssenc
	Database string // all, replication, or specific database name
	User     string // all, or specific user name
	Address  string // IP address (required for host types)
}

// HbaFileManager manages pg_hba.conf file content,
// providing methods to read, modify, and persist changes with backup support.
type HbaFileManager struct {
	FilePath   string // path to the pg_hba.conf file
	RawContent string // original content for backup
	Content    string // modified content pending flush
}

// NewHbaFileManager reads the specified pg_hba.conf file and returns a manager instance.
func NewHbaFileManager(file string) (*HbaFileManager, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	return &HbaFileManager{
		FilePath:   file,
		RawContent: string(content),
		Content:    string(content),
	}, nil
}

// Update updates the authentication method for a matching HbaRecord.
// If no matching record is found, a new entry is appended.
// pg_hba.conf format: TYPE DATABASE USER [ADDRESS] METHOD [auth-options]
func (m *HbaFileManager) Update(target HbaRecord, newMethod string) {
	lines := strings.Split(m.Content, "\n")
	found := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		fields := strings.Fields(trimmed)
		// minimum fields: TYPE DATABASE USER METHOD (local type)
		// host types need: TYPE DATABASE USER ADDRESS METHOD
		if len(fields) < 4 {
			continue
		}

		// check if this line matches the target record
		isMatch := false
		if fields[0] == target.Type &&
			fields[1] == target.Database &&
			fields[2] == target.User {
			if target.Type == "local" {
				isMatch = true
			} else if len(fields) >= 5 && fields[3] == target.Address {
				isMatch = true
			}
		}

		if isMatch {
			// construct new line with updated method
			if target.Type == "local" {
				lines[i] = fmt.Sprintf(
					"%-10s %-10s %-10s %-10s",
					fields[0],
					fields[1],
					fields[2],
					newMethod,
				)
			} else {
				lines[i] = fmt.Sprintf(
					"%-10s %-10s %-10s %-20s %-10s",
					fields[0],
					fields[1],
					fields[2],
					fields[3],
					newMethod,
				)
			}
			found = true
		}
	}

	// append new entry if not found
	if !found {
		var newLine string
		if target.Type == "local" {
			newLine = fmt.Sprintf(
				"%-10s %-10s %-10s %-10s",
				target.Type,
				target.Database,
				target.User,
				newMethod,
			)
		} else {
			newLine = fmt.Sprintf(
				"%-10s %-10s %-10s %-20s %-10s",
				target.Type,
				target.Database,
				target.User,
				target.Address,
				newMethod,
			)
		}
		lines = append(lines, newLine)
	}

	m.Content = strings.Join(lines, "\n")
}

// Flush writes modified content to disk with backup.
// Creates a timestamped backup of original content before overwriting.
// Returns nil if no changes detected.
func (m *HbaFileManager) Flush() error {
	if m.RawContent == m.Content {
		return nil
	}
	backPath := fmt.Sprintf("%s_%s", m.FilePath, time.Now().Format("20060102.150405"))
	if err := os.WriteFile(backPath, []byte(m.RawContent), 0640); err != nil {
		return err
	}
	return os.WriteFile(m.FilePath, []byte(m.Content), 0640)
}

// jsonDecodeFromFile reads filename and JSON-decodes it into v.
// A missing file yields an error wrapping os.ErrNotExist, so callers
// can branch with os.IsNotExist (the keeper relies on this on first run).
func jsonDecodeFromFile(filename string, v interface{}) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

// jsonEncodeToFileIndent marshals v as indented JSON (2-space) and writes
// it to filename with the given permission. The write is atomic: data is
// written to a temp file in the same directory and renamed into place, so
// a crash or a concurrent reader can never observe a partial state file.
func jsonEncodeToFileIndent(filename string, v interface{}, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(filename)
	tf, err := os.CreateTemp(dir, ".tmp-json-*")
	if err != nil {
		return err
	}
	tmp := tf.Name()
	defer os.Remove(tmp) // no-op once Rename succeeds (tmp no longer exists)

	if _, err := tf.Write(data); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, filename)
}
