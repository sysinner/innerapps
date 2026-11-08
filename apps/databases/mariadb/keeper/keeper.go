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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sysinner/innerstack/v2/pkg/inconf"
)

var (
	mdbMemMin = int32(32) // in MiB

	// cfgMemMiB is the memory budget of this instance in MiB, sized in do()
	// from the replica memory limit and memory_usage_limit; it drives the
	// innodb_buffer_pool_size rendered into my.cnf.
	cfgMemMiB int64

	cfgLast EnvConfig

	appCfr inconf.AppReplicaHelper
)

// Fixed roots, mirroring the postgresql keeper layout. Persistent artifacts
// (datadir, my.cnf, server log) are version-scoped under
// /opt/mariadb/mariadb<major>/ so data bootstrapped by one major series is
// never silently opened by another; control-plane files (lock, pid, state)
// are shared, since only one major can run at a time (single port/socket).
const (
	mdbHome    = "/opt/mariadb"
	mdbPidFile = mdbHome + "/mariadbd.pid"
	mdbState   = mdbHome + "/keeper/init_option.json"
	mdbLock    = mdbHome + "/keeper.lock"
)

// mdbMajor is the detected major series of the installed mariadbd (e.g.
// "11"), set once in do(): the Alpine package is not major-versioned, so
// the installed binary is the only source of truth.
var mdbMajor string

// mdbVerPath is the version-scoped root, mirroring the postgresql keeper's
// postgresql<ver>/ layout.
func mdbVerPath() string { return filepath.Join(mdbHome, "mariadb"+mdbMajor) }

// mdbDataDir is the live datadir.
func mdbDataDir() string { return filepath.Join(mdbVerPath(), "data") }

// mdbStage is the staging dir the bootstrap is published into, renamed to
// mdbDataDir on success.
func mdbStage() string { return mdbDataDir() + ".bootstrap" }

// mdbConf is the server configuration file.
func mdbConf() string { return filepath.Join(mdbVerPath(), "my.cnf") }

// mdbSrvLog is the mariadbd stdout/stderr log.
func mdbSrvLog() string { return filepath.Join(mdbVerPath(), "server_logfile.log") }

// mdbInstallTimeout bounds the system-table bootstrap: on a slow host it can
// exceed 5 minutes, and killing a healthy bootstrap mid-run leaves debris
// that breaks retries.
const mdbInstallTimeout = 15 * time.Minute

// mdbSocket is the unix socket mariadbd listens on (my.cnf "socket") and the
// socket every client tool connects through (--socket=...). The two MUST
// agree: the tools open "<mdbSocket>" directly, so a mismatch surfaces as
// "Can't connect to local server through socket ...".
const mdbSocket = "/tmp/mysql.sock"

// mdbPort is the TCP port mariadbd listens on; it must match the
// [[service_ports]] entry in app-spec.toml so dependents reach the instance.
const mdbPort = 3306

// Binary paths: the install task installs the Alpine mariadb / mariadb-client
// packages on the pinned base image, which place all tools under /usr/bin
// with the modern (mariadb-*) names.
const (
	binInstallDB = "/usr/bin/mariadb-install-db"
	binServer    = "/usr/bin/mariadbd"
	binClient    = "/usr/bin/mariadb"
	binAdmin     = "/usr/bin/mariadb-admin"
)

// EnvConfig is the persisted state (init_option.json): what earlier runs
// have already reconciled. It holds only fields later runs branch on; the
// my.cnf on disk is the source of truth for applied sizing.
type EnvConfig struct {
	Inited bool `json:"inited"`

	// MajorVer records the mariadb major series (e.g. "11") this instance
	// was bootstrapped on; a drifted binary is refused instead of opening
	// another major's datadir.
	MajorVer string `json:"major_ver,omitempty"`

	// RootAuth marks the one-time anonymous-account hardening as done.
	RootAuth string `json:"root_auth"`

	// Databases records every provisioned database, the main database from
	// db_name / db_user / db_auth included, normalized to the same shape as
	// the "databases" array_group entries.
	Databases []EnvConfigDatabase `json:"databases,omitempty"`

	// Updated stamps the bootstrap attempt for diagnosis.
	Updated int64 `json:"updated"`
}

// EnvConfigDatabase is one provisioned database together with the account
// that owns it (an empty Owner means no dedicated account is provisioned).
type EnvConfigDatabase struct {
	Name string `json:"name"`

	// Owner is the mariadb account granted full privileges on this database.
	Owner string `json:"owner,omitempty"`

	// Auth is the password of the Owner account.
	Auth string `json:"auth,omitempty"`
}

// DatabaseSync upserts a database entry by name.
func (cfg *EnvConfig) DatabaseSync(item EnvConfigDatabase) {

	for i, v := range cfg.Databases {
		if v.Name == item.Name {
			cfg.Databases[i] = item
			return
		}
	}

	cfg.Databases = append(cfg.Databases, item)
}

// DatabaseGet returns the database entry for name, or nil.
func (cfg *EnvConfig) DatabaseGet(name string) *EnvConfigDatabase {

	for _, v := range cfg.Databases {
		if v.Name == name {
			return &v
		}
	}

	return nil
}

func main() {
	// The full config dump carries plaintext passwords: keep it behind an
	// explicit opt-in.
	if os.Getenv("KEEPER_DEBUG") != "" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	// --shutdown is the teardown entry used by the shutdown task in
	// app-spec.toml: it delegates graceful stop to the keeper itself so the
	// binary path, socket and auth model live in one place.
	if len(os.Args) > 1 && os.Args[1] == "--shutdown" {
		shutdown()
		return
	}

	if err := lockInstance(); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			slog.Info("another keeper instance is running, skip")
			return
		}
		slog.Error("lock keeper", "err", err.Error())
		os.Exit(1)
	}

	if err := do(); err != nil {
		slog.Error("setup failed", "err", err.Error())
		os.Exit(1)
	}
}

// shutdown is the best-effort teardown path: errors are logged but never
// fail the task, so instance teardown is never blocked (a server that is
// not running or not reachable falls through to the executor's kill).
func shutdown() {
	if err := lockInstance(); err != nil {
		slog.Info("skip shutdown", "err", err.Error())
		return
	}
	if err := stop(); err != nil {
		slog.Error("shutdown mariadbd", "err", err.Error())
	}
}

// lockInstance takes the process-lifetime flock. The run task and the
// keepalive cron can both exec the keeper as separate processes, and an
// overlapping run would race on the bootstrap staging dir and on server
// control, so a second keeper exits immediately instead. The lock fd is
// held for the whole process lifetime and released by the kernel on exit.
// It is the only serialization point: the keeper itself is single-goroutine.
func lockInstance() error {
	f, err := os.OpenFile(mdbLock, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", mdbLock, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return err
	}
	return nil
}

// reMdbVer parses mariadbd --version output, e.g. "Ver 11.4.5-MariaDB".
var reMdbVer = regexp.MustCompile(`Ver (\d+)\.`)

// detectMajor asks the installed server binary for its major series. The
// Alpine package is not major-versioned (unlike postgresql18), so the
// binary is the only source of truth for which series this instance runs.
func detectMajor() (string, error) {
	out, err := runCmd(15*time.Second, binServer, "--version")
	if err != nil {
		return "", fmt.Errorf("probe mariadbd version: %w", err)
	}
	m := reMdbVer.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cannot parse mariadbd version from %q", string(out))
	}
	return string(m[1]), nil
}

func do() error {

	if _, err := os.Stat(binServer); err != nil {
		return fmt.Errorf("mariadbd not found, install task did not run: %w", err)
	}

	major, err := detectMajor()
	if err != nil {
		return err
	}
	mdbMajor = major

	tstart := time.Now()

	helper, err := inconf.NewAppReplicaHelper()
	if err != nil {
		return fmt.Errorf("inconf setup: %w", err)
	}
	appCfr = helper

	slog.Debug("load config", "cfg", appCfr.ReplicaInstance().App.Deploy.Configs)

	// MemoryLimit is in bytes, convert to MiB
	if appCfr.ReplicaInstance().App.Deploy.MemoryLimit > 0 {
		cfgMemMiB = appCfr.ReplicaInstance().App.Deploy.MemoryLimit / (1 << 20)
	}

	if item := appCfr.ConfigItem("memory_usage_limit"); item != nil {

		ramPc64, _ := strconv.ParseInt(item.Value, 10, 64)

		if ramPc64 < 10 || ramPc64 > 100 {
			return errors.New("invalid memory_usage_limit setup")
		}

		ramPc := (cfgMemMiB * ramPc64) / 100
		if offset := ramPc % int64(mdbMemMin); offset > 0 {
			ramPc -= offset
		}
		if ramPc < int64(mdbMemMin) {
			ramPc = int64(mdbMemMin)
		}
		if ramPc < cfgMemMiB {
			cfgMemMiB = ramPc
		}

	} else {
		return errors.New("config item memory_usage_limit not found")
	}

	if cfgMemMiB < int64(mdbMemMin) {
		return errors.New("not enough memory to fit this MariaDB instance")
	}

	dbs, err := loadDatabases()
	if err != nil {
		return err
	}
	slog.Info("config loaded", "mariadb", mdbMajor,
		"databases", len(dbs), "mem_mib", cfgMemMiB)

	if err := jsonDecodeFromFile(mdbState, &cfgLast); err != nil &&
		!os.IsNotExist(err) {
		return fmt.Errorf("load init_option.json: %w", err)
	}

	// Major-version guard: a drifted binary (base image refresh shipping a
	// new mariadb series) must refuse rather than open, or silently
	// re-bootstrap beside, a datadir of another major. A major upgrade is
	// an explicit migration (dump/restore), never a side effect of an
	// image refresh; patch-level drift within the major is fine.
	if cfgLast.Inited && cfgLast.MajorVer != "" && cfgLast.MajorVer != mdbMajor {
		return fmt.Errorf(
			"mariadb major version drifted %s -> %s: data of %s lives at %s, migrate it explicitly before running %s",
			cfgLast.MajorVer, mdbMajor, cfgLast.MajorVer, mdbDataDir(), mdbMajor)
	}

	if err := os.MkdirAll(mdbVerPath(), 0750); err != nil {
		return err
	}

	// my.cnf is rendered before the datadir bootstrap so the system-table
	// installer and the server read the same deterministic configuration.
	confChanged, err := initConf()
	if err != nil {
		return fmt.Errorf("init_conf: %w", err)
	}

	if err := initDataDir(); err != nil {
		return fmt.Errorf("init_datadir: %w", err)
	}

	// restart only when my.cnf changed under a running server
	if confChanged && pidof() > 0 {
		if err := stop(); err != nil {
			return fmt.Errorf("stop mariadbd: %w", err)
		}
	}

	if err := start(); err != nil {
		return fmt.Errorf("start mariadbd: %w", err)
	}

	if err := initRootAuth(); err != nil {
		return fmt.Errorf("init_root_auth: %w", err)
	}

	if err := initDatabases(dbs); err != nil {
		return fmt.Errorf("init_databases: %w", err)
	}

	slog.Info("setup complete", "duration", time.Since(tstart))
	return nil
}

// loadDatabases builds the desired database list from the live config: the
// main database (db_name / db_user / db_auth) normalized to the same shape
// as the "databases" array_group entries, followed by those entries. The
// deploy-time validator (unique="app") already guarantees array_group db_user
// values are distinct from the main db_user and from each other.
func loadDatabases() ([]EnvConfigDatabase, error) {

	var main EnvConfigDatabase

	if item := appCfr.ConfigItem("db_name"); item != nil {
		main.Name = item.Value
	} else {
		return nil, errors.New("config item db_name not found")
	}

	itemUser := appCfr.ConfigItem("db_user")
	itemAuth := appCfr.ConfigItem("db_auth")
	if itemUser == nil {
		return nil, errors.New("config item db_user not found")
	}
	if itemAuth == nil {
		return nil, errors.New("config item db_auth not found")
	}
	main.Owner = itemUser.Value
	main.Auth = itemAuth.Value

	dbs := []EnvConfigDatabase{main}

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
			if db.Name == "" || db.Name == main.Name {
				continue
			}
			dbs = append(dbs, db)
		}
	}

	// Database / user names are interpolated into SQL statements executed as
	// the local root: reject anything outside [0-9a-zA-Z_] up front instead
	// of escaping it.
	for _, db := range dbs {
		if !validIdent(db.Name) {
			return nil, fmt.Errorf("invalid identifier %q, expect [0-9a-zA-Z_]{1,64}", db.Name)
		}
		if db.Owner != "" && !validIdent(db.Owner) {
			return nil, fmt.Errorf("invalid identifier %q, expect [0-9a-zA-Z_]{1,64}", db.Owner)
		}
	}

	return dbs, nil
}

// renderMyCnf renders the full server configuration. The keeper is the only
// writer: manual edits are overwritten on the next run (a timestamped backup
// is kept), so this function stays pure and the whole step idempotent.
// There is deliberately no [client] section: every tool call passes
// --socket / -u explicitly, and this file is not on the clients' default
// search path, so a [client] block here would affect nobody.
func renderMyCnf() string {
	return fmt.Sprintf(`[mysqld]
datadir = %s
socket = %s
pid-file = %s
port = %d
bind-address = 0.0.0.0
skip-name-resolve
character-set-server = utf8mb4
collation-server = utf8mb4_general_ci
# sized from the instance memory limit, see memory_usage_limit
innodb_buffer_pool_size = %dM
`,
		mdbDataDir(), mdbSocket, mdbPidFile, mdbPort, cfgMemMiB)
}

// innodbBootstrapOverrides relaxes InnoDB durability for the one-shot system
// table bootstrap (see initDataDir); never used by the running server.
const innodbBootstrapOverrides = `
[mysqld]
innodb_flush_log_at_trx_commit = 0
skip-innodb-doublewrite
`

// initConf writes my.cnf when the rendered content differs from what is on
// disk (timestamped backup of the previous version first). It reports whether
// the content changed, so the caller can restart a running server.
func initConf() (bool, error) {

	content := renderMyCnf()

	prev, err := os.ReadFile(mdbConf())
	if err == nil {

		if string(prev) == content {
			return false, nil
		}

		backPath := fmt.Sprintf("%s.%s", mdbConf(), time.Now().Format("20060102.150405"))
		if err := os.WriteFile(backPath, prev, 0640); err != nil {
			return false, err
		}

	} else if !os.IsNotExist(err) {
		return false, err
	}

	if err := os.WriteFile(mdbConf(), []byte(content), 0640); err != nil {
		return false, err
	}

	return true, nil
}

func initDataDir() error {

	if cfgLast.Inited {
		return nil
	}

	// A present system schema means the data dir was initialized before
	// (possibly by a keeper run whose state file was lost): never re-install
	// over existing data.
	if _, err := os.Stat(filepath.Join(mdbDataDir(), "mysql")); err == nil {
		cfgLast.Inited = true
		return saveState()
	} else if !os.IsNotExist(err) {
		return err
	}

	// Defense in depth for a lost state file: datadir of another major
	// series present on the volume means this instance was inited before,
	// so bootstrapping fresh here would reset the app to empty and orphan
	// the old data. Restore the state file or migrate explicitly instead.
	if dirs, err := filepath.Glob(filepath.Join(mdbHome, "mariadb*")); err == nil {
		for _, dir := range dirs {
			if filepath.Base(dir) == "mariadb"+mdbMajor {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "data", "mysql")); err == nil {
				return fmt.Errorf(
					"datadir of another mariadb major exists at %s but the keeper state is missing, restore %s or migrate explicitly",
					dir, mdbState)
			}
		}
	}

	// A datadir without the mysql system schema cannot hold user data: the
	// server never ran on it. Whatever is there is debris from an interrupted
	// bootstrap (e.g. a truncated ibdata1, which aborts every retry with
	// "wrong space ID"), so drop it and bootstrap fresh. The bootstrap runs
	// on container-local storage, is copied into the volume staging dir and
	// renamed into place only on success, so an interrupted run leaves no
	// half-initialized datadir.
	if err := os.RemoveAll(mdbDataDir()); err != nil {
		return err
	}
	if err := os.RemoveAll(mdbStage()); err != nil {
		return err
	}

	// Stamp the bootstrap attempt into the state file for diagnosis (the
	// stamp write also proves the volume is writable before the long run),
	// including the major series that owns this datadir from now on.
	cfgLast.MajorVer = mdbMajor
	cfgLast.Updated = time.Now().UnixMilli()
	if err := saveState(); err != nil {
		return err
	}

	// Bootstrap into container-local storage: a bootstrap is thousands of
	// small synced writes, which is pathologically slow on volume mounts
	// (bind-mounted host dirs in particular). Container-local disk does it
	// in seconds; only the finished datadir then crosses to the volume as
	// one sequential copy.
	localStage := "/tmp/mariadb-keeper-bootstrap"
	if err := os.RemoveAll(localStage); err != nil {
		return err
	}
	defer os.RemoveAll(localStage)
	if err := os.MkdirAll(localStage, 0750); err != nil {
		return err
	}

	// Durability is disabled for the one-shot bootstrap: the local staging
	// datadir is discarded when interrupted, so crash safety buys nothing,
	// while the ~2000 system-table commit fsyncs dominate the runtime on
	// slow storage. The server keeps the fully durable my.cnf.
	bootCnf := mdbConf() + ".bootstrap"
	if err := os.WriteFile(bootCnf,
		[]byte(renderMyCnf()+innodbBootstrapOverrides), 0640); err != nil {
		return err
	}
	defer os.Remove(bootCnf)

	// --defaults-file must be the first option; "normal" leaves root as a
	// passwordless local account (unix socket / localhost only), business
	// users are provisioned separately for network access. The explicit
	// --datadir overrides the [mysqld] datadir from the defaults file, so
	// the bootstrap writes into the local staging dir.
	out, err := runCmd(mdbInstallTimeout, binInstallDB,
		"--defaults-file="+bootCnf,
		"--datadir="+localStage,
		"--auth-root-authentication-method=normal",
		"--skip-test-db")
	if err != nil {
		slog.Error("mariadb-install-db", "err", err.Error(), "output", string(out))
		return err
	}
	slog.Info("mariadb-install-db ok")

	// Publish: copy the finished datadir into the volume staging dir, then
	// rename it into place (rename is atomic within the volume filesystem).
	// busybox cp -a streams and preserves modes; the target was already
	// cleared above, so it cannot be copied into itself.
	if out, err := runCmd(10*time.Minute, "cp", "-a", localStage, mdbStage()); err != nil {
		slog.Error("publish bootstrap datadir", "err", err.Error(), "output", string(out))
		return fmt.Errorf("publish bootstrap datadir: %w", err)
	}
	if err := os.Rename(mdbStage(), mdbDataDir()); err != nil {
		return fmt.Errorf("promote bootstrap datadir: %w", err)
	}

	cfgLast.Inited = true
	return saveState()
}

func cleanRunlock() {
	os.Remove(mdbPidFile)
	os.Remove(mdbSocket)
}

func start() error {

	if !cfgLast.Inited {
		return errNotInited
	}

	if pidof() > 0 {
		return nil
	}

	cleanRunlock()

	lf, err := os.OpenFile(mdbSrvLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer lf.Close()

	slog.Info("start mariadbd")

	cmd := exec.Command(binServer, "--defaults-file="+mdbConf())
	cmd.Stdout = lf
	cmd.Stderr = lf
	// Detach the server as its own session leader so it survives the keeper
	// and is not taken down with the keeper's process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		slog.Error("start mariadbd", "err", err.Error())
		return err
	}
	// No Wait() on purpose: the server is a daemon, readiness is verified
	// below through the admin ping instead of process reaping.

	if waitUntil(30*time.Second, serverReady) {
		slog.Info("start mariadbd ok", "pid", cmd.Process.Pid)
		return nil
	}

	return errors.New("mariadbd not ready in 30s, see " + mdbSrvLog())
}

func stop() error {

	if pidof() == 0 {
		return nil
	}

	out, err := runCmd(90*time.Second, binAdmin,
		"--socket="+mdbSocket, "-u", "root", "shutdown")
	if err != nil {
		slog.Error("shutdown mariadbd", "err", err.Error(), "output", string(out))
		return err
	}

	// mariadb-admin shutdown waits for exit; this poll is a bounded safeguard.
	if waitUntil(30*time.Second, func() bool { return pidof() == 0 }) {
		slog.Info("shutdown mariadbd ok")
		return nil
	}

	return errors.New("mariadbd not stopped in 30s")
}

// waitUntil polls cond about once per second until it holds or the timeout
// lapses.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	for i := 0; i < int(timeout/time.Second); i++ {
		if cond() {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// pidof returns the mariadbd pid from the pid file, or 0 when the file is
// absent or the process no longer exists (stale after a crash).
func pidof() int {

	b, err := os.ReadFile(mdbPidFile)
	if err != nil {
		return 0
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}

	if p, err := os.FindProcess(pid); err == nil {
		if err := p.Signal(syscall.Signal(0)); err == nil {
			return pid
		}
	}

	return 0
}

func serverReady() bool {
	_, err := runCmd(5*time.Second, binAdmin,
		"--socket="+mdbSocket, "-u", "root", "ping")
	return err == nil
}

// initRootAuth is a one-time hardening right after the first server start:
// drop the anonymous local accounts the installer may have created, so the
// only reachable logins are the provisioned business accounts ('%' hosts with
// passwords). DROP USER IF EXISTS is a no-op when they are already gone.
func initRootAuth() error {

	if !cfgLast.Inited {
		return errNotInited
	}

	if cfgLast.RootAuth != "" {
		return nil
	}

	// One client run for all three accounts: batch mode stops at the first
	// failing statement and reports its line.
	stmt := "DROP USER IF EXISTS ''@'localhost'; " +
		"DROP USER IF EXISTS ''@'127.0.0.1'; " +
		"DROP USER IF EXISTS ''@'::1'"
	if out, err := sqlExec(stmt); err != nil {
		slog.Error("drop anonymous user", "err", err.Error(), "output", string(out))
		return err
	}

	cfgLast.RootAuth = "1"
	return saveState()
}

// initDatabases reconciles every desired database (the main database and the
// "databases" array_group entries alike): it creates the database and
// provisions its owner account (with password and full privileges on that
// database). The whole step is idempotent: an entry already recorded in
// cfgLast with an unchanged owner/auth is skipped, and every statement is
// IF NOT EXISTS / self-converging so partial reruns converge.
func initDatabases(dbs []EnvConfigDatabase) error {

	if !cfgLast.Inited {
		return errNotInited
	}

	for _, db := range dbs {

		// skip if already provisioned and unchanged
		if prev := cfgLast.DatabaseGet(db.Name); prev != nil &&
			prev.Owner == db.Owner && prev.Auth == db.Auth {
			continue
		}

		if err := createDatabase(db.Name); err != nil {
			return err
		}

		if db.Owner != "" {
			if err := provisionUser(db.Owner, db.Auth, db.Name); err != nil {
				return err
			}
		}

		slog.Info("provision database ok", "name", db.Name, "owner", db.Owner)

		cfgLast.DatabaseSync(db)
		if err := saveState(); err != nil {
			return err
		}
	}

	return nil
}

// createDatabase ensures the database exists with the instance charset.
func createDatabase(name string) error {
	stmt := "CREATE DATABASE IF NOT EXISTS " + quoteIdent(name) +
		" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci"
	if out, err := sqlExec(stmt); err != nil {
		slog.Error("create database", "name", name,
			"err", err.Error(), "output", string(out))
		return err
	}
	return nil
}

// provisionUser ensures an account exists, sets its password and grants it
// full privileges on dbname. The statements run as one batch: the client
// stops at the first failing statement and reports its line.
func provisionUser(user, auth, dbname string) error {

	uh := sqlUserHost(user)

	stmt := "CREATE USER IF NOT EXISTS " + uh
	if auth != "" {
		stmt += "; ALTER USER " + uh + " IDENTIFIED BY " + quoteStr(auth)
	}
	stmt += "; GRANT ALL PRIVILEGES ON " + quoteIdent(dbname) + ".* TO " + uh

	// stmt content is not logged: it carries the account password
	if out, err := sqlExec(stmt); err != nil {
		slog.Error("provision user", "user", user,
			"err", err.Error(), "output", string(out))
		return err
	}

	slog.Info("provision user ok", "user", user)
	return nil
}

// sqlExec runs SQL statements through the local client as the passwordless
// local root over the unix socket, bounded by a timeout so a wedged client
// cannot pin the keeper.
func sqlExec(stmt string) ([]byte, error) {
	return runCmd(15*time.Second, binClient,
		"--socket="+mdbSocket, "-u", "root", "-e", stmt)
}

// reIdent constrains user-provided database / account names: they are
// interpolated into SQL run as root, so anything outside [0-9a-zA-Z_]{1,64}
// is rejected rather than escaped.
var reIdent = regexp.MustCompile(`^[0-9a-zA-Z_]{1,64}$`)

func validIdent(s string) bool {
	return reIdent.MatchString(s)
}

// quoteIdent wraps a SQL identifier in backticks (backticks are already
// rejected by validIdent, so no doubling is needed).
func quoteIdent(s string) string {
	return "`" + s + "`"
}

// sqlStrReplacer escapes a SQL string literal: quotes are doubled and
// backslashes escaped, so a user-provided password cannot break out.
var sqlStrReplacer = strings.NewReplacer(`\`, `\\`, `'`, `''`)

func quoteStr(s string) string {
	return "'" + sqlStrReplacer.Replace(s) + "'"
}

// sqlUserHost formats an account opened for remote access ('%' host), which
// is how provisioned business users are reached through service port 3306.
func sqlUserHost(user string) string {
	return quoteStr(user) + "@'%'"
}

// runCmd runs name with args, returning combined stdout+stderr, and kills the
// child process group after timeout (the error is context.DeadlineExceeded).
// Every mariadb tool call goes through here so a wedged child cannot stall
// the keeper. WaitDelay is needed because install-db spawns helpers that
// inherit the output pipes: killing only the direct child would leave those
// pipes open, and CombinedOutput would block on them forever.
func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	// The child runs in its own process group and a timeout kills the whole
	// group: install-db is a shell script, and killing only the script would
	// orphan its bootstrap mariadbd child, which keeps writing into the
	// staging dir and piles up across retries.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd.CombinedOutput()
}

// errNotInited guards steps that require the datadir bootstrap to have run.
var errNotInited = errors.New("mariadb datadir not initialized")

// saveState atomically persists the reconciled state to mdbState.
func saveState() error {
	return jsonEncodeToFileIndent(mdbState, cfgLast, 0644)
}

// jsonDecodeFromFile reads filename and JSON-decodes it into v.
// A missing file yields an error wrapping os.ErrNotExist, so callers
// can branch with os.IsNotExist (the keeper relies on this on first run).
func jsonDecodeFromFile(filename string, v any) error {
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
func jsonEncodeToFileIndent(filename string, v any, perm os.FileMode) error {
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
