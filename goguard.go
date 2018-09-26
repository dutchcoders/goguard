package main

// partially based on CompileDaemon
// https://github.com/githubnemo/CompileDaemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	glob "github.com/ryanuber/go-glob"

	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
)

const WorkDelay = 3500

type globList []string

func (g *globList) Matches(value string) bool {
	for _, v := range *g {
		if match := glob.Glob(v, value); match {
			return true
		}
	}
	return false
}

func kill(process *os.Process) error {
	// https://github.com/golang/go/issues/8854
	pgid, err := syscall.Getpgid(process.Pid)
	if err != nil {
		return err
	}

	syscall.Kill(-pgid, syscall.SIGTERM)

	waiter := make(chan struct{})
	go func() {
		process.Wait()
		waiter <- struct{}{}
	}()

	select {
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, color.RedString("Killing unresponding processes. We've asked them nicely once before."))
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		return err
	case <-waiter:
	}

	return nil
}

func NewColoredWriter(w io.Writer, color int) ColoredWriter {
	return ColoredWriter{Writer: w, color: color}
}

type ColoredWriter struct {
	io.Writer
	color int
}

func (cw ColoredWriter) Write(p []byte) (n int, err error) {
	cw.Writer.Write([]byte(fmt.Sprintf("\033[%dm", cw.color)))
	n, err = cw.Writer.Write(p)
	cw.Writer.Write([]byte("\033[0m"))
	return
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("goguard")
		fmt.Println("\nUsage: ")
		fmt.Println("goguard go run server/main.go -port 127.0.0.1:8081")
		os.Exit(0)
	}

	events := make(chan string)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	terminating := make(chan os.Signal, 1)

	signal.Notify(terminating, os.Interrupt)
	signal.Notify(terminating, syscall.SIGTERM)

	// go restarter(events, restart)

	defer func() {
		fmt.Fprintln(os.Stderr, color.YellowString("go-guard stopped"))
	}()

	go func() {
		defer func() {
			stopped <- struct{}{}
		}()

		var wg sync.WaitGroup
		for {
			var err error
			defer func() {
				if err != nil {
					fmt.Println(err)
				}
			}()

			fmt.Fprintln(os.Stderr, color.YellowString("[goguard] starting: "), strings.Join(os.Args[1:], " "))

			cmd := exec.Command(os.Args[1], os.Args[2:]...)

			var stdout io.ReadCloser
			if stdout, err = cmd.StdoutPipe(); err != nil {
				err = fmt.Errorf("can't get stdout pipe for command: %s", err)
				return
			}

			var stderr io.ReadCloser
			if stderr, err = cmd.StderrPipe(); err != nil {
				err = fmt.Errorf("can't get stderr pipe for command: %s", err)
				return
			}

			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				fmt.Printf("Error occured during start: %s\n", err.Error())
				continue
			}

			go io.Copy(os.Stdout, stdout)
			go io.Copy(NewColoredWriter(os.Stderr, 91), stderr)

			stopped := make(chan error)

			wg.Add(1)
			go func() {
				defer wg.Done()

				err := cmd.Wait()

				select {
				case stopped <- err:
				default:
				}
			}()

			// wait for message to restart
			wg.Add(1)
			go func() {
				defer wg.Done()

				var threshold <-chan time.Time

				for {
					select {
					case <-events:
						threshold = time.After(time.Duration(WorkDelay * time.Millisecond))
					case <-threshold:
						fmt.Fprintln(os.Stderr, color.YellowString(fmt.Sprintf("[goguard] Changes detected. Restarting.")))
						kill(cmd.Process)
						return
					case s := <-terminating:
						_ = s

						pgid, err := syscall.Getpgid(cmd.Process.Pid)
						if err != nil {
							fmt.Println("Error pgid cmd")
							return
						}

						syscall.Kill(-pgid, syscall.SIGTERM)

						fmt.Fprintln(os.Stderr, color.YellowString(fmt.Sprintf("[goguard] Terminating application. Restarting.")))

						// wait for process to finish clean
						select {
						case <-time.After(time.Second * 2):
							// force after two seconds
							kill(cmd.Process)
							return
						case <-terminating:
							fmt.Println("[goguard] yeah, yeah, quitting.")
							// twice ctrl + c
							os.Exit(0)
						}
					case <-stop:
						kill(cmd.Process)
						return
					case err := <-stopped:
						if err != nil {
							fmt.Fprintln(os.Stderr, color.RedString(fmt.Sprintf("[goguard] Error: %s", err)))

							for {
								select {
								case <-time.After(time.Second * 5):
									return
								case <-terminating:
									select {
									case <-time.After(time.Second * 2):
										// force after two seconds
										return
									case <-terminating:
										fmt.Println("[goguard] yeah, yeah, quitting.")
										// twice ctrl + c
										os.Exit(0)
									}
								case <-events:
									threshold = time.After(time.Duration(WorkDelay * time.Millisecond))
								case <-threshold:
									kill(cmd.Process)
									return
								}
							}
						} else {
							// when unexpected quit, wait restart / stop again
							fmt.Fprintln(os.Stderr, color.YellowString(fmt.Sprintf("Process finished clean.")))

							select {
							case <-time.After(time.Second * 2):
								return
							case <-terminating:
								fmt.Println("Yeah, yeah, quitting.")
								// twice ctrl + c
								os.Exit(0)
							}
						}
					}
				}
			}()

			wg.Wait()
		}
	}()

	// this will start the watcher. It will watch for Modified events
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	defer watcher.Close()

	flag_excludedFiles := globList([]string{".git", ".gopath", "node_modules", "bower_components", "Godeps", "cache.db", "vendor", "cache"})
	patterns := globList([]string{"**.go", "**.html"})

	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			if flag_excludedFiles.Matches(info.Name()) {
				return filepath.SkipDir
			}

			return watcher.Add(path)
		}
		return err
	})

	if err != nil {
		log.Fatal("filepath.Walk():", err)
	}

	for {
		select {
		case ev := <-watcher.Events:
			if ev.Name == "" {
				continue
			}

			name := path.Clean(ev.Name)
			if flag_excludedFiles.Matches(name) {
				continue
			}

			if strings.HasPrefix(path.Base(name), ".") {
				continue
			}

			if !patterns.Matches(name) {
				continue
			}

			fmt.Println(ev.Name)

			select {
			case events <- ev.Name:
			default:
			}
		case err := <-watcher.Errors:
			if v, ok := err.(*os.SyscallError); ok {
				if v.Err == syscall.EINTR {
					continue
				}
				log.Fatal("watcher.Error: SyscallError:", v)
			}
			log.Fatal("watcher.Error:", err)
		case <-stopped:
			watcher.Close()
			return
		}
	}
}
