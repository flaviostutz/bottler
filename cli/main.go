package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/flaviostutz/perfstat"
	"github.com/flaviostutz/perfstat/detectors"
	"github.com/mum4k/termdash"
	"github.com/mum4k/termdash/container"
	"github.com/mum4k/termdash/keyboard"
	"github.com/mum4k/termdash/terminal/termbox"
	"github.com/mum4k/termdash/terminal/terminalapi"
	"github.com/sirupsen/logrus"
)

type Option struct {
	freq         float64
	sensibility  float64
	promBindHost string
	promBindPort uint
	promPath     string
}

type screen interface {
	rootContainer() container.Option
	update(opt Option, ps *perfstat.Perfstat, paused bool, term *termbox.Terminal) error
	onEvent(evt *terminalapi.Keyboard)
}

var (
	opt                Option
	rootc              *container.Container
	ps                 *perfstat.Perfstat
	curScreenCtxCancel context.CancelFunc
	controller         *termdash.Controller
	curScreen          screen
	paused             bool
	t                  *termbox.Terminal
	screens            map[string]screen
)

func main() {
	loglevel := logrus.ErrorLevel
	screens = make(map[string]screen)

	flag.Float64Var(&opt.freq, "freq", 0.0, "Analysis frequency. Changes data capture and display refresh frequency. Higher consumes more CPU. Defaults to 0 (automatic depending on sensibility)")
	flag.Float64Var(&opt.sensibility, "sensibility", 1.0, "Lower values (ex.: 0.2) means larger timespan in analysis, leading to more accurate results but slower responses. Higher values (ex.: 5) means short time analysis but may lead to false positives. Defaults to 1.0 which means detecting a continuous 100% CPU in 30s")

	promf := flag.NewFlagSet("prometheus", flag.ExitOnError)
	promf.Float64Var(&opt.freq, "freq", 0.0, "Analysis frequency. Changes data capture and display refresh frequency. Higher consumes more CPU. Defaults to 1 Hz")
	promf.Float64Var(&opt.sensibility, "sensibility", 1.0, "Lower values (ex.: 0.2) means larger timespan in analysis, leading to more accurate results but slower responses. Higher values (ex.: 5) means short time analysis but may lead to false positives. Defaults to 1.0 which means detecting a continuous 100% CPU in 30s")
	promf.UintVar(&opt.promBindPort, "port", 8880, "Prometheus exporter port. defaults to 8880")
	promf.StringVar(&opt.promBindHost, "host", "0.0.0.0", "Prometheus exporter bind host. defaults to 0.0.0.0")
	promf.StringVar(&opt.promPath, "path", "/metrics", "Prometheus exporter port. defaults to /metric")

	logrus.SetLevel(loglevel)

	prom := false
	if len(os.Args) > 1 && os.Args[1] == "prometheus" {
		err := promf.Parse(os.Args[2:])
		if err != nil {
			panic(err)
		}
		prom = true
	} else {
		flag.Parse()
	}

	if opt.freq != 0.0 && (opt.freq > 5 || opt.freq < 0.05) {
		panic("--freq must be between 0.05 and 5")
	}

	if opt.sensibility > 30 || opt.sensibility < 0.01 {
		panic("--sensibility must be between 0.01 and 30")
	}

	logrus.Debugf("Initializing Perfstat engine")
	opt2 := detectors.NewOptions()
	dur := time.Duration(30/opt.sensibility) * time.Second
	opt2.CPULoadAvgDuration = dur
	opt2.IORateLoadDuration = dur
	opt2.IOLimitsSpan = dur
	opt2.MemAvgDuration = dur
	opt2.MemLeakDuration = (dur * 10)

	opt2.DefaultSampleFreq = opt.freq
	if opt2.DefaultSampleFreq == 0.0 {
		//set freq=0.03 for sensibility=0.01
		//set freq=5 for sensibility=20
		opt2.DefaultSampleFreq = (0.1657*opt.sensibility + 0.02834)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ps = perfstat.Start(ctx, opt2)
	ps.SetLogLevel(loglevel)
	// time.Sleep(6 * time.Second)

	if !prom {
		startUI(ctx, cancel, math.Round(opt2.DefaultSampleFreq*2.0)+1.0)
	} else {
		logrus.Debugf("Starting Prometheus Exporter")
		startPrometheus(ctx, opt, ps)
	}
}

func startUI(ctx context.Context, cancel context.CancelFunc, screenFreq float64) {
	logrus.Debugf("Initializing UI...")

	var err error
	t, err = termbox.New(termbox.ColorMode(terminalapi.ColorMode256))
	if err != nil {
		panic(err)
	}

	rootc, err = container.New(t, container.ID("root"))
	if err != nil {
		panic(err)
	}

	evtHandler := func(k *terminalapi.Keyboard) {
		if curScreen != nil {
			curScreen.onEvent(k)
		}
		//exit
		if k.Key == keyboard.KeyCtrlC || k.Key == 113 {
			cancel()
			//pause/unpause
		} else if k.Key == 80 || k.Key == 112 {
			paused = !paused
		} else if k.Key == keyboard.KeyEsc || k.Key == 68 || k.Key == 72 || k.Key == 104 || k.Key == 96 || k.Key == 49 {
			showScreen("home")
		} else if k.Key == 50 {
			showScreen("cpu")
		} else if k.Key == 51 {
			showScreen("mem")
		} else if k.Key == 52 {
			showScreen("disk")
		} else if k.Key == 53 {
			showScreen("net")
		}
		updateScreens()
	}

	controller, err = termdash.NewController(t, rootc, termdash.KeyboardSubscriber(evtHandler))
	if err != nil {
		panic(err)
	}

	//prepare screens
	h, err := newHome(opt, ps)
	if err != nil {
		panic(fmt.Sprintf("Error preparing screen. err=%s", err))
	}
	screens["home"] = h

	d, err := newDetails("cpu", opt, ps)
	if err != nil {
		panic(fmt.Sprintf("Error preparing screen. err=%s", err))
	}
	screens["cpu"] = d

	d, err = newDetails("mem", opt, ps)
	if err != nil {
		panic(fmt.Sprintf("Error preparing screen. err=%s", err))
	}
	screens["mem"] = d

	d, err = newDetails("disk", opt, ps)
	if err != nil {
		panic(fmt.Sprintf("Error preparing screen. err=%s", err))
	}
	screens["disk"] = d

	d, err = newDetails("net", opt, ps)
	if err != nil {
		panic(fmt.Sprintf("Error preparing screen. err=%s", err))
	}
	screens["net"] = d

	showScreen("home")

	paused = false

	ticker := time.NewTicker(time.Duration(1000/screenFreq) * time.Millisecond).C
	for {
		select {

		case <-ctx.Done():
			//this is used because when using controller some "double closing" arises
			//should be removed after fixing termdash (https://github.com/mum4k/termdash/issues/241)
			defer func() {
				recover()
			}()
			t.Close()

		case <-ticker:
			err := updateScreens()
			if err != nil {
				panic(fmt.Sprintf("Error updating screens. err=%s", err))
			}
		}
	}
}

func showScreen(name string) {
	s, ok := screens[name]
	if !ok {
		panic(fmt.Sprintf("Screen not found. name=%s", name))
	}
	rootc.Update("root", s.rootContainer())
	curScreen = s

	updateScreens()
}

func updateScreens() error {
	for _, v := range screens {
		err := v.update(opt, ps, paused, t)
		if err != nil {
			return err
		}
	}
	return controller.Redraw()
}
