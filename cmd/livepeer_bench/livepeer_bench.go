package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	//"runtime/pprof"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/m3u8"
	"github.com/olekukonko/tablewriter"
)

func main() {
	// Override the default flag set since there are dependencies that
	// incorrectly add their own flags (specifically, due to the 'testing'
	// package being linked)
	flag.Set("logtostderr", "true")
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	in := flag.String("in", "", "Input m3u8 manifest file")
	live := flag.Bool("live", true, "Simulate live stream")
	concurrentSessions := flag.Int("concurrentSessions", 1, "# of concurrent transcode sessions")
	repeat := flag.Int("repeat", 1, "# of times benchmark will be repeated")
	segs := flag.Int("segs", 0, "Maximum # of segments to transcode (default all)")
	log := flag.Int("log", 3, "Log level (1 fatal, 2 error, 3 warning, 4 info, 5 verbose, 6 debug, 7 trace, default warning)")
	transcodingOptions := flag.String("transcodingOptions", "P240p30fps16x9,P360p30fps16x9,P720p30fps16x9", "Transcoding options for broadcast job, or path to json config")
	nvidia := flag.String("nvidia", "", "Comma-separated list of Nvidia GPU device IDs (or \"all\" for all available devices)")
	netint := flag.String("netint", "", "Comma-separated list of NetInt device GUIDs (or \"all\" for all available devices)")
	outPrefix := flag.String("outPrefix", "", "Output segments' prefix (no segments are generated by default)")
	concurrentSessionDelay := flag.Duration("concurrentSessionDelay", 300*time.Millisecond, "Delay before starting a new concurrent session")
	sign := flag.Bool("mpeg7Sign", false, "Calculate MPEG-7 video signature while transcoding")

	flag.Parse()

	if *in == "" {
		glog.Errorf("Please provide the input manifest as `%s -in <input.m3u8>`", os.Args[0])
		flag.Usage()
		os.Exit(1)
	}

	profiles := parseVideoProfiles(*transcodingOptions)

	f, err := os.Open(*in)
	if err != nil {
		glog.Exit("Couldn't open input manifest: ", err)
	}
	p, _, err := m3u8.DecodeFrom(bufio.NewReader(f), true)
	if err != nil {
		glog.Exit("Couldn't decode input manifest: ", err)
	}
	pl, ok := p.(*m3u8.MediaPlaylist)
	if !ok {
		glog.Exitf("Expecting media playlist in the input %s", *in)
	}

	accel := ffmpeg.Software
	devices := []string{}
	if *nvidia != "" {
		var err error
		accel = ffmpeg.Nvidia
		devices, err = common.ParseAccelDevices(*nvidia, accel)
		if err != nil {
			glog.Exitf("Error while parsing '-nvidia %v' flag: %v", *nvidia, err)
		}
	}

	if *netint != "" {
		var err error
		accel = ffmpeg.Netint
		devices, err = common.ParseAccelDevices(*netint, accel)
		if err != nil {
			glog.Exitf("Error while parsing '-netint %v' flag: %v", *netint, err)
		}
	}

	glog.Infof("log level is: %d", ffmpeg.LogLevel(*log*8))
	ffmpeg.InitFFmpegWithLogLevel(ffmpeg.LogLevel(*log * 8))

	var wg sync.WaitGroup
	dir := path.Dir(*in)

	table := tablewriter.NewWriter(os.Stderr)
	data := [][]string{
		{"Source File", *in},
		{"Transcoding Options", *transcodingOptions},
		{"Concurrent Sessions", fmt.Sprintf("%v", *concurrentSessions)},
		{"Live Mode", fmt.Sprintf("%v", *live)},
		{"MPEG-7 Sign Mode", fmt.Sprintf("%v", *sign)},
	}

	if accel == ffmpeg.Nvidia && len(devices) > 0 {
		data = append(data, []string{"Nvidia GPU IDs", fmt.Sprintf("%v", strings.Join(devices, ","))})
	}

	if accel == ffmpeg.Netint && len(devices) > 0 {
		data = append(data, []string{"Netint GUIDs", fmt.Sprintf("%v", strings.Join(devices, ","))})
	}

	fmt.Printf("data %s \n", data)

	if *repeat > 1 {
		data = append(data, []string{"Repeat Times", fmt.Sprintf("%v", *repeat)})
	}

	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("*")
	table.SetColumnSeparator("|")
	table.AppendBulk(data)
	table.Render()

	ffmpeg.InitFFmpegWithLogLevel(ffmpeg.FFLogWarning)
	fmt.Println("timestamp,session,segment,seg_dur,transcode_time,frames")

	var mu sync.Mutex
	for repeatCounter := 0; repeatCounter < *repeat; repeatCounter++ {
		segCount := 0
		realTimeSegCount := 0
		srcDur := 0.0
		transcodeDur := 0.0
		for i := 0; i < *concurrentSessions; i++ {
			wg.Add(1)
			go func(k int, wg *sync.WaitGroup) {
				var tc *ffmpeg.Transcoder = ffmpeg.NewTranscoder()
				for j, v := range pl.Segments {
					iterStart := time.Now()
					if *segs > 0 && j >= *segs {
						break
					}
					if v == nil {
						continue
					}
					u := path.Join(dir, v.URI)
					in := &ffmpeg.TranscodeOptionsIn{
						Fname: u,
						Accel: accel,
					}
					if ffmpeg.Software != accel {
						in.Device = devices[k%len(devices)]
						fmt.Printf("in.Device %s \n", in.Device)
					}
					profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
						opts := []ffmpeg.TranscodeOptions{}
						for n, p := range profs {
							oname := ""
							muxer := ""
							if *outPrefix != "" {
								oname = fmt.Sprintf("%s_%s_%d_%d_%d.ts", *outPrefix, p.Name, n, k, j)
								muxer = "mpegts"
							} else {
								oname = "-"
								muxer = "null"
							}
							o := ffmpeg.TranscodeOptions{
								Oname:        oname,
								Profile:      p,
								Accel:        accel,
								AudioEncoder: ffmpeg.ComponentOptions{Name: "copy"},
								Muxer:        ffmpeg.ComponentOptions{Name: muxer},
								CalcSign:     *sign,
							}
							opts = append(opts, o)
						}
						return opts
					}
					out := profs2opts(profiles)
					t := time.Now()
					res, err := tc.Transcode(in, out)
					end := time.Now()
					if err != nil {
						glog.Exitf("Transcoding failed for session %d segment %d: %v", k, j, err)
					}
					fmt.Printf("%s,%d,%d,%0.4v,%0.4v,%v\n", end.Format("2006-01-02 15:04:05.9999"), k, j, v.Duration, end.Sub(t).Seconds(), res.Encoded[0].Frames)
					segTxDur := end.Sub(t).Seconds()
					mu.Lock()
					transcodeDur += segTxDur
					srcDur += v.Duration
					segCount++
					if segTxDur <= v.Duration {
						realTimeSegCount += 1
					}
					mu.Unlock()
					iterEnd := time.Now()
					segDur := time.Duration(v.Duration * float64(time.Second))
					if *live {
						time.Sleep(segDur - iterEnd.Sub(iterStart))
					}
				}
				tc.StopTranscoder()
				wg.Done()
			}(i, &wg)
			time.Sleep(*concurrentSessionDelay) // wait for at least one segment before moving on to the next session
		}
		wg.Wait()
		if segCount == 0 || srcDur == 0.0 {
			glog.Exit("Input manifest has no segments or total duration is 0s")
		}
		statsTable := tablewriter.NewWriter(os.Stderr)
		stats := [][]string{
			{"Concurrent Sessions", fmt.Sprintf("%v", *concurrentSessions)},
			{"Total Segs Transcoded", fmt.Sprintf("%v", segCount)},
			{"Real-Time Segs Transcoded", fmt.Sprintf("%v", realTimeSegCount)},
			{"* Real-Time Segs Ratio *", fmt.Sprintf("%0.4v", float64(realTimeSegCount)/float64(segCount))},
			{"Total Source Duration", fmt.Sprintf("%vs", srcDur)},
			{"Total Transcoding Duration", fmt.Sprintf("%vs", transcodeDur)},
			{"* Real-Time Duration Ratio *", fmt.Sprintf("%0.4v", transcodeDur/srcDur)},
		}

		statsTable.SetAlignment(tablewriter.ALIGN_LEFT)
		statsTable.SetCenterSeparator("*")
		statsTable.SetColumnSeparator("|")
		statsTable.AppendBulk(stats)
		statsTable.Render()
	}
}

func parseVideoProfiles(inp string) []ffmpeg.VideoProfile {
	profiles := []ffmpeg.VideoProfile{}
	if inp != "" {
		// try opening up json file with profiles
		content, err := ioutil.ReadFile(inp)
		if err == nil && len(content) > 0 {
			// parse json profiles
			var parsingError error
			profiles, parsingError = ffmpeg.ParseProfiles(content)
			if parsingError != nil {
				glog.Exit(parsingError)
			}
		} else {
			// check the built-in profiles
			profiles = make([]ffmpeg.VideoProfile, 0)
			presets := strings.Split(inp, ",")
			for _, v := range presets {
				if p, ok := ffmpeg.VideoProfileLookup[strings.TrimSpace(v)]; ok {
					profiles = append(profiles, p)
				}
			}
		}
		if len(profiles) <= 0 {
			glog.Exitf("No transcoding options provided")
		}
	}
	return profiles
}
