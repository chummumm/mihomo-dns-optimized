package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/metacubex/mihomo/common/cmd"
	"github.com/metacubex/mihomo/component/age"
	"github.com/metacubex/mihomo/component/generator"
	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/constant/features"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/rules/provider"

	"go.uber.org/automaxprocs/maxprocs"
)

var (
	version                       bool
	testConfig                    bool
	geodataMode                   bool
	homeDir                       string
	configFile                    string
	configString                  string
	configBytes                   []byte
	ageSecretKey                  string
	externalUI                    string
	externalController            string
	externalControllerTLS         string
	externalControllerUnix        string
	externalControllerPipe        string
	externalControllerRoutingMark int
	secret                        string
	postUp                        string
	postDown                      string
)

func getIntEnv(key string) int {
	value := os.Getenv(key)
	intValue, _ := strconv.Atoi(value)
	return intValue
}

func init() {
	flag.StringVar(&homeDir, "d", os.Getenv("CLASH_HOME_DIR"), "set configuration directory")
	flag.StringVar(&configFile, "f", os.Getenv("CLASH_CONFIG_FILE"), "specify configuration file")
	flag.StringVar(&configString, "config", os.Getenv("CLASH_CONFIG_STRING"), "specify base64-encoded configuration string")
	flag.StringVar(&ageSecretKey, "age-secret-key", os.Getenv("CLASH_AGE_SECRET_KEY"), "specify age secret key to decrypt configuration")
	flag.StringVar(&externalUI, "ext-ui", os.Getenv("CLASH_OVERRIDE_EXTERNAL_UI_DIR"), "deprecated and ignored; external UI is fixed at <home>/ui")
	flag.StringVar(&externalController, "ext-ctl", os.Getenv("CLASH_OVERRIDE_EXTERNAL_CONTROLLER"), "override external controller address")
	flag.StringVar(&externalControllerTLS, "ext-ctl-tls", os.Getenv("CLASH_OVERRIDE_EXTERNAL_CONTROLLER_TLS"), "override external controller tls address")
	flag.StringVar(&externalControllerUnix, "ext-ctl-unix", os.Getenv("CLASH_OVERRIDE_EXTERNAL_CONTROLLER_UNIX"), "override external controller unix address")
	flag.StringVar(&externalControllerPipe, "ext-ctl-pipe", os.Getenv("CLASH_OVERRIDE_EXTERNAL_CONTROLLER_PIPE"), "override external controller pipe address")
	flag.IntVar(&externalControllerRoutingMark, "ext-ctl-routing-mark", getIntEnv("CLASH_OVERRIDE_EXTERNAL_CONTROLLER_ROUTING_MARK"), "override external controller routing mark")
	flag.StringVar(&secret, "secret", os.Getenv("CLASH_OVERRIDE_SECRET"), "override secret for RESTful API")
	flag.StringVar(&postUp, "post-up", os.Getenv("CLASH_POST_UP"), "set post-up script")
	flag.StringVar(&postDown, "post-down", os.Getenv("CLASH_POST_DOWN"), "set post-down script")
	flag.BoolVar(&geodataMode, "m", false, "set geodata mode")
	flag.BoolVar(&version, "v", false, "show current version of mihomo")
	flag.BoolVar(&testConfig, "t", false, "test configuration and exit")
}

func main() {
	flag.Parse()
	// Defensive programming: panic when code mistakenly calls net.DefaultResolver
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		//panic("should never be called")
		buf := make([]byte, 1024)
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				buf = buf[:n]
				break
			}
			buf = make([]byte, 2*len(buf))
		}
		fmt.Fprintf(os.Stderr, "panic: should never be called\n\n%s", buf) // always print all goroutine stack
		os.Exit(2)
		return nil, nil
	}

	_, _ = maxprocs.Set(maxprocs.Logger(func(string, ...any) {}))

	if len(os.Args) > 1 && os.Args[1] == "convert-ruleset" {
		provider.ConvertMain(os.Args[2:])
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "generate" {
		generator.Main(os.Args[2:])
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "age" {
		age.Main(os.Args[2:])
		return
	}

	if version {
		fmt.Printf("%s %s %s with %s %s\n",
			C.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(), C.BuildTime)
		if tags := features.Tags(); len(tags) != 0 {
			fmt.Printf("Use tags: %s\n", strings.Join(tags, ", "))
		}

		return
	}

	if homeDir != "" {
		if !filepath.IsAbs(homeDir) {
			currentDir, _ := os.Getwd()
			homeDir = filepath.Join(currentDir, homeDir)
		}
		C.SetHomeDir(homeDir)
	}

	if geodataMode {
		geodata.SetGeodataMode(true)
	}

	if ageSecretKey != "" {
		if err := age.VeritySecretKeys(ageSecretKey); err != nil {
			log.Errorln("Parse age-secret-key error: %s", err.Error())
		}
		age.SetGlobalSecretKeys(ageSecretKey)
	}

	if configString != "" {
		var err error
		configBytes, err = base64.StdEncoding.DecodeString(configString)
		if err != nil {
			log.Fatalln("Initial configuration error: %s", err.Error())
			return
		}
	} else if configFile == "-" {
		var err error
		configBytes, err = io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalln("Initial configuration error: %s", err.Error())
			return
		}
	} else {
		if configFile != "" {
			if !filepath.IsAbs(configFile) {
				currentDir, _ := os.Getwd()
				configFile = filepath.Join(currentDir, configFile)
			}
		} else {
			configFile = filepath.Join(C.Path.HomeDir(), C.Path.Config())
		}
		C.SetConfig(configFile)

		if err := config.Init(C.Path.HomeDir()); err != nil {
			log.Fatalln("Initial configuration directory error: %s", err.Error())
		}
	}

	if testConfig {
		if len(configBytes) != 0 {
			if _, err := executor.ParseWithBytes(configBytes); err != nil {
				log.Errorln(err.Error())
				fmt.Println("configuration test failed")
				os.Exit(1)
			}
		} else {
			if _, err := executor.Parse(); err != nil {
				log.Errorln(err.Error())
				fmt.Printf("configuration file %s test failed\n", C.Path.Config())
				os.Exit(1)
			}
		}
		fmt.Printf("configuration file %s test is successful\n", C.Path.Config())
		return
	}

	var options []hub.Option
	if externalUI != "" {
		options = append(options, hub.WithExternalUI(externalUI))
	}
	if externalController != "" {
		options = append(options, hub.WithExternalController(externalController))
	}
	if externalControllerTLS != "" {
		options = append(options, hub.WithExternalControllerTLS(externalControllerTLS))
	}
	if externalControllerUnix != "" {
		options = append(options, hub.WithExternalControllerUnix(externalControllerUnix))
	}
	if externalControllerPipe != "" {
		options = append(options, hub.WithExternalControllerPipe(externalControllerPipe))
	}
	if externalControllerRoutingMark != 0 {
		options = append(options, hub.WithExternalControllerRoutingMark(externalControllerRoutingMark))
	}
	if secret != "" {
		options = append(options, hub.WithSecret(secret))
	}

	if err := runDaemon(options); err != nil {
		// runDaemon has completed cleanup before reporting startup failure.
		log.Fatalln("%s", err)
	}
}

func runDaemon(options []hub.Option) error {
	termSign := make(chan os.Signal, 1)
	hupSign := make(chan os.Signal, 1)
	// Queue stop requests before configuration can create managed resources.
	// systemd may stop a Type=simple service while its startup is still running.
	signal.Notify(termSign, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(hupSign, syscall.SIGHUP)
	defer signal.Stop(termSign)
	defer signal.Stop(hupSign)
	stopRequests, stopWatching := relayStopSignals(termSign, executor.RequestShutdown)
	defer stopWatching()
	configured := false
	defer func() {
		shutdownUntilComplete(executor.Shutdown, stopRequests)
		if configured && postDown != "" {
			if _, err := cmd.ExecShell(postDown); err != nil {
				log.Errorln("post-down script error: %s", err.Error())
			}
		}
	}()
	if err := hub.Parse(configBytes, options...); err != nil {
		if errors.Is(err, executor.ErrShuttingDown) {
			return nil
		}
		return fmt.Errorf("parse config error: %w", err)
	}
	configured = true
	select {
	case <-stopRequests:
		return nil
	default:
	}
	if postUp != "" {
		if _, err := cmd.ExecShell(postUp); err != nil {
			return fmt.Errorf("post-up script error: %w", err)
		}
	}
	for {
		select {
		case <-stopRequests:
			return nil
		case <-hupSign:
			if err := hub.Parse(configBytes, options...); err != nil {
				if errors.Is(err, executor.ErrShuttingDown) {
					return nil
				}
				log.Errorln("Parse config error: %s", err.Error())
			}
		}
	}
}

// Publish stop intent independently of configuration parsing and the executor
// lock, then wake the existing exit/retry loop. Repeated signals never block
// this relay while the main goroutine is busy with startup or cleanup.
func relayStopSignals(received <-chan os.Signal, publish func()) (<-chan os.Signal, func()) {
	requests := make(chan os.Signal, 1)
	cancel, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-cancel:
				return
			case sig, ok := <-received:
				if !ok {
					return
				}
				publish()
				select {
				case requests <- sig:
				default:
				}
			}
		}
	}()
	return requests, func() { close(cancel); <-done }
}

// A failed graceful cleanup must not become a successful process exit. Keep
// runtime ownership available for retries; only an external forced termination
// can abandon it. The service manager retains control of its stop timeout.
func shutdownUntilComplete(stop func() error, signals <-chan os.Signal) {
	delay := 100 * time.Millisecond
	for {
		if err := stop(); err == nil {
			return
		} else {
			log.Errorln("Shutdown incomplete; keeping the process alive and retrying owned resource cleanup: %s", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-signals:
		case <-timer.C:
		}
		timer.Stop()
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}
