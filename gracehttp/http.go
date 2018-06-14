// Package gracehttp provides easy to use graceful restart
// functionality for HTTP server.
package gracehttp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/facebookgo/httpdown"
	"github.com/sunreaver/grace/gracenet"
)

var (
	logger     *log.Logger
	didInherit = os.Getenv("LISTEN_FDS") != ""
	ppid       = os.Getppid()
)

type option func(*app)

// An app contains one or more servers and associated configuration.
type app struct {
	servers         []*http.Server
	http            *httpdown.HTTP
	net             *gracenet.Net
	listeners       []net.Listener
	sds             []httpdown.Server
	preStartProcess func() error
	sufStartProcess func() error
	errors          chan error
	waitWG          *sync.WaitGroup
	restartSig      os.Signal
}

func newApp(servers []*http.Server) *app {
	return &app{
		servers:   servers,
		http:      &httpdown.HTTP{},
		net:       &gracenet.Net{},
		listeners: make([]net.Listener, 0, len(servers)),
		sds:       make([]httpdown.Server, 0, len(servers)),

		preStartProcess: func() error { return nil },
		sufStartProcess: func() error { return nil },
		// 2x num servers for possible Close or Stop errors + 1 for possible
		// StartProcess error.
		errors: make(chan error, 1+(len(servers)*2)),
	}
}

func (a *app) listen() error {
	for _, s := range a.servers {
		// TODO: default addresses
		l, err := a.net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		if s.TLSConfig != nil {
			l = tls.NewListener(l, s.TLSConfig)
		}
		a.listeners = append(a.listeners, l)
	}
	return nil
}

func (a *app) serve() {
	for i, s := range a.servers {
		a.sds = append(a.sds, a.http.Serve(s, a.listeners[i]))
	}
}

func (a *app) wait() {
	a.waitWG = &sync.WaitGroup{}
	a.waitWG.Add(len(a.sds) * 2) // Wait & Stop
	go a.signalHandler(a.waitWG)
	for _, s := range a.sds {
		go func(s httpdown.Server) {
			defer a.waitWG.Done()
			if err := s.Wait(); err != nil {
				a.errors <- err
			}
		}(s)
	}
	a.waitWG.Wait()
}

func (a *app) term(wg *sync.WaitGroup) {
	for _, s := range a.sds {
		go func(s httpdown.Server) {
			defer wg.Done()
			if err := s.Stop(); err != nil {
				a.errors <- err
			}
		}(s)
	}
}

func (a *app) signalHandler(wg *sync.WaitGroup) {
	ch := make(chan os.Signal, 10)
	signal.Notify(ch, a.restartSig)
	for {
		<-ch
		signal.Stop(ch)
		err := a.preStartProcess()
		if err != nil {
			a.errors <- err
		}
		// we only return here if there's an error, otherwise the new process
		// will send us a TERM when it's ready to trigger the actual shutdown.
		if pid, err := a.net.StartProcess(); err != nil {
			a.errors <- err
		} else {
			if logger != nil {
				logger.Printf("New pid %d.", pid)
			}
			a.term(a.waitWG)
			a.waitWG.Wait()
			err = a.sufStartProcess()
			if err != nil {
				a.errors <- err
			}
			return
		}
	}
}

func (a *app) run() error {
	// Acquire Listeners
	if err := a.listen(); err != nil {
		return err
	}

	// Some useful logging.
	if logger != nil {
		if didInherit {
			if ppid == 1 {
				logger.Printf("Listening on init activated %s", pprintAddr(a.listeners))
			} else {
				const msg = "Graceful handoff of %s with new pid %d and old pid %d"
				logger.Printf(msg, pprintAddr(a.listeners), os.Getpid(), ppid)
			}
		} else {
			const msg = "Serving %s with pid %d"
			logger.Printf(msg, pprintAddr(a.listeners), os.Getpid())
		}
	}

	// Start serving.
	a.serve()

	// Close the parent if we inherited and it wasn't init that started us.
	if didInherit && ppid != 1 {
		if err := syscall.Kill(ppid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("failed to close parent: %s", err)
		}
	}

	waitdone := make(chan struct{})
	go func() {
		defer close(waitdone)
		a.wait()
	}()

	select {
	case err := <-a.errors:
		if err == nil {
			panic("unexpected nil error")
		}
		if logger != nil {
			logger.Printf("Exiting err %v.", err)
		}
		return err
	case <-waitdone:
		if logger != nil {
			logger.Printf("Exiting pid %d.", os.Getpid())
		}
		return nil
	}
}

// ServeWithOptions does the same as Serve, but takes a set of options to
// configure the app struct.
func ServeWithOptions(restartSig os.Signal, servers []*http.Server, options ...option) error {
	a := newApp(servers)
	a.restartSig = restartSig
	for _, opt := range options {
		opt(a)
	}
	return a.run()
}

// Serve will serve the given http.Servers.
// restartSig: 当进程接收到系统信号，会进行平滑重启操作
func Serve(restartSig os.Signal, servers ...*http.Server) error {
	a := newApp(servers)
	a.restartSig = restartSig
	return a.run()
}

// PreStartProcess configures a callback to trigger during graceful restart
// directly before starting the successor process. This allows the current
// process to release holds on resources that the new process will need.
func PreStartProcess(hook func() error) option {
	return func(a *app) {
		a.preStartProcess = hook
	}
}

// SufStartProcess configures a callback to trigger during graceful restart
// directly end started the successor process.
func SufStartProcess(hook func() error) option {
	return func(a *app) {
		a.sufStartProcess = hook
	}
}

// Used for pretty printing addresses.
func pprintAddr(listeners []net.Listener) []byte {
	var out bytes.Buffer
	for i, l := range listeners {
		if i != 0 {
			fmt.Fprint(&out, ", ")
		}
		fmt.Fprint(&out, l.Addr())
	}
	return out.Bytes()
}

// SetLogger sets logger to be able to grab some useful logs
func SetLogger(l *log.Logger) {
	logger = l
}
