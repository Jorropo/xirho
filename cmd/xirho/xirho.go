// The xirho command implements a basic renderer using xirho.
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"time"

	"golang.org/x/image/draw"

	"github.com/zephyrtronium/xirho"
	"github.com/zephyrtronium/xirho/encoding"
	"github.com/zephyrtronium/xirho/encoding/flame"
)

func main() {
	var intr bool
	var outname, profname, inname, flamename string
	var sigint bool
	var timeout time.Duration
	var width, height int
	var osa int
	var tm xirho.ToneMap
	var resample string
	var procs int
	var echo bool
	var bgr, bgg, bgb, bga int
	flag.BoolVar(&intr, "i", false, "interactive mode")
	flag.StringVar(&outname, "png", "", "output filename (default stdout)")
	flag.StringVar(&profname, "prof", "", "CPU profile output (default no profiling)")
	flag.StringVar(&inname, "in", "", "input json filename (default stdin)")
	flag.StringVar(&flamename, "flame", "", "input flame filename")
	flag.BoolVar(&sigint, "C", true, "save image on interrupt instead of exiting (ignored when interactive)")
	flag.DurationVar(&timeout, "dur", 0, "max duration to render (default ignored; always ignored when interactive)")
	flag.IntVar(&width, "width", 1024, "output image width")
	flag.IntVar(&height, "height", 1024, "output image height")
	flag.IntVar(&osa, "osa", 1, "oversampling; histogram bins per pixel per axis")
	flag.Float64Var(&tm.Gamma, "gamma", 1, "gamma factor")
	flag.Float64Var(&tm.GammaMin, "thresh", 0, "gamma threshold")
	flag.Float64Var(&tm.Brightness, "bright", 1, "brightness")
	flag.StringVar(&resample, "resample", "catmull-rom", "resampling method (catmull-rom, bilinear, approx-bilinear, or nearest)")
	flag.IntVar(&procs, "procs", runtime.GOMAXPROCS(0), "concurrent render routines")
	flag.BoolVar(&echo, "echo", false, "print system encoding before rendering")
	flag.IntVar(&bgr, "bg.r", 0, "background red (0-255)")
	flag.IntVar(&bgg, "bg.g", 0, "background green (0-255)")
	flag.IntVar(&bgb, "bg.b", 0, "background blue (0-255)")
	flag.IntVar(&bga, "bg.a", 255, "background alpha (0-255)")
	flag.Parse()
	resampler := resamplers[resample]
	if resampler == nil {
		log.Fatalln("no resampler named", resample)
	}
	if profname != "" {
		prof, err := os.Create(profname)
		if err != nil {
			log.Fatal(err)
		}
		if err = pprof.StartCPUProfile(prof); err != nil {
			panic(err)
		}
		defer pprof.StopCPUProfile()
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 && !intr {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	if sigint && !intr {
		ctx, cancel = context.WithCancel(ctx)
		ch := make(chan os.Signal)
		signal.Notify(ch, os.Interrupt)
		go func() {
			<-ch
			signal.Reset(os.Interrupt)
			cancel()
		}()
	}

	var system xirho.System
	var r *xirho.Render
	var err error
	u := color.NRGBA{
		R: uint8(bgr),
		G: uint8(bgg),
		B: uint8(bgb),
		A: uint8(bga),
	}
	if intr {
		switch {
		case inname != "":
			f, err := os.Open(inname)
			if err != nil {
				log.Fatalln("error opening input:", err)
			}
			d := json.NewDecoder(f)
			d.UseNumber()
			s, err := encoding.Unmarshal(d)
			if err != nil {
				log.Fatalln("error unmarshaling system:", err)
			}
			system = s.System
		case flamename != "":
			f, err := os.Open(flamename)
			if err != nil {
				log.Fatalln("error opening input:", err)
			}
			d := xml.NewDecoder(f)
			flm, err := flame.Unmarshal(d)
			if err != nil {
				log.Fatalln("error unmarshaling system:", err)
			}
			system = flm.System
			r = flm.R
		}
		interactive(ctx, r, system, width, height, resampler, tm, u, osa, procs)
		return
	}
	if flamename == "" {
		var in io.Reader = os.Stdin
		if inname != "" {
			f, err := os.Open(inname)
			if err != nil {
				log.Fatalln("error opening input:", err)
			}
			in = f
		}
		d := json.NewDecoder(in)
		d.UseNumber()
		s, err := encoding.Unmarshal(d)
		if err != nil {
			log.Fatalln("error unmarshaling system:", err)
		}
		system = s.System
	} else {
		in, err := os.Open(flamename)
		if err != nil {
			log.Fatalln("error opening input:", err)
		}
		d := xml.NewDecoder(in)
		flm, err := flame.Unmarshal(d)
		if err != nil {
			log.Fatalln("error unmarshaling system:", err)
		}
		system = flm.System
		r = flm.R
	}
	log.Println("allocating histogram, estimated", xirho.HistMem(width*osa, height*osa)>>20, "MB")
	r.Hist.Reset(width*osa, height*osa)
	if echo {
		m, err := encoding.Marshal(system, r, tm, nil, nil)
		if err != nil {
			log.Fatalln("error reading system from input:", err)
		}
		log.Printf("system:\n%s\n", m)
	}
	log.Println("rendering for", timeout, "or until ^C")
	r.Render(ctx, system, procs)
	log.Println("finished render with", r.Iters(), "iters,", r.Hits(), "hits")
	signal.Reset(os.Interrupt) // no rendering for ^C to interrupt
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(u), image.Point{}, draw.Src)
	log.Printf("drawing onto image of size %dx%d", width, height)
	src := r.Hist.Image(tm, r.Area(), r.Iters(), osa)
	resampler.Scale(img, img.Bounds(), src, src.Bounds(), draw.Over, nil)
	out := os.Stdout
	if outname != "" {
		log.Println("encoding to", outname)
		var err error
		out, err = os.Create(outname)
		if err != nil {
			log.Fatalln("error creating output file:", err)
		}
		defer out.Close()
	} else {
		log.Println("encoding to stdout")
	}
	err = png.Encode(out, img)
	if err != nil {
		log.Fatalln("error encoding image:", err)
	}
}

var resamplers = map[string]draw.Scaler{
	"catmull-rom":     draw.CatmullRom,
	"bilinear":        draw.BiLinear,
	"approx-bilinear": draw.ApproxBiLinear,
	"nearest":         draw.NearestNeighbor,
	"lanczos1":        lanczos(1),
	"lanczos3":        lanczos(3),
	"lanczos5":        lanczos(5),
}

func lanczos(a float64) *draw.Kernel {
	return &draw.Kernel{
		Support: a,
		At: func(x float64) float64 {
			if x == 0 {
				return 1
			}
			return a * math.Sin(math.Pi*x) * math.Sin(math.Pi*x/a) / (math.Pi * math.Pi * x * x)
		},
	}
}
