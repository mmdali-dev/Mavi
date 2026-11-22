package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"mavi"
)

func env(k, v string) string {
	if a := os.Getenv(k); a != "" {
		return a
	}
	return v
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "staff-path" {
		var b [16]byte
		rand.Read(b[:])
		fmt.Println("/" + hex.EncodeToString(b[:]))
		return
	}
	// Deployment files live beside the executable, regardless of the launch directory.
	exe, err := os.Executable()
	if err != nil || os.Chdir(filepath.Dir(exe)) != nil {
		fmt.Fprintln(os.Stderr, "Could not open the deployment directory.")
		os.Exit(1)
	}
	if err := mavi.PrepareDeployment(); err != nil {
		fmt.Fprintln(os.Stderr, "Could not initialize deployment files:", err)
		os.Exit(1)
	}
	if err := loadEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "Could not read .env in the deployment directory.")
		os.Exit(1)
	}
	addr := os.Getenv("MAVI_ADDR")
	if addr == "" {
		port, err := strconv.ParseUint(env("MAVI_PORT", "8080"), 10, 16)
		if err != nil || port == 0 {
			fmt.Fprintln(os.Stderr, "MAVI_PORT must be a number between 1 and 65535.")
			os.Exit(1)
		}
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}
	home, e := os.UserConfigDir()
	if e != nil {
		os.Exit(1)
	}
	s, e := mavi.New(mavi.Config{Dir: env("MAVI_DATA", filepath.Join(home, "mavi-data")), Staff: os.Getenv("MAVI_STAFF_PATH"), Tables: env("MAVI_TABLES", "1-12,15"), Seed: os.Getenv("MAVI_SEED") == "1", TrustedProxy: os.Getenv("MAVI_TRUSTED_PROXY")})
	if e != nil {
		fmt.Fprintln(os.Stderr, "Startup failed: check configuration, data directory and migration.")
		os.Exit(1)
	}
	defer s.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	listening := make(chan error, 1)
	go func() {
		listening <- s.App.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	select {
	case e = <-listening:
	case <-signals:
		e = s.App.ShutdownWithTimeout(10 * time.Second)
		if e == nil {
			e = <-listening
		}
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, "Listener or shutdown failed.")
		os.Exit(1)
	}
}

// loadEnv reads literal MAVI_* defaults. An explicitly set environment variable wins, even if empty.
func loadEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(s.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return errors.New("invalid .env assignment")
		}
		if !strings.HasPrefix(k, "MAVI_") {
			continue
		}
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return s.Err()
}
