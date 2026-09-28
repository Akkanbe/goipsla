// show track (namespace "track").
//
//declscope:namespace track

package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
	"goipsla/internal/config"
)

//declscope:package // the show command adds it as a subcommand
func newShowTrackCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "track [<n>]",
		Short: "Show the tracks (like show track)",
		Long: "With a track number, show that track the way Cisco show track does. Without,\n" +
			"list every track, one per line.",
		Args: trackArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := 0
			if len(args) == 1 {
				id, _ = strconv.Atoi(args[0])
			}
			tracks, err := newClient(opts).Tracks(cmd.Context(), id)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				if id != 0 && len(tracks) == 1 {
					return writeJSON(w, tracks[0])
				}
				return writeJSON(w, tracks)
			}
			if len(tracks) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no tracks")
				return nil
			}
			if id != 0 {
				return writeTrack(w, tracks[0])
			}
			tw := fmtTable(w)
			fmt.Fprintln(tw, "TRACK\tOP\tMODE\tSTATE\tCHANGES\tLAST-CHANGE\tRC\tRTT(ms)")
			for _, t := range tracks {
				state := t.State
				if t.Pending != "" {
					state += "->" + t.Pending
				}
				rtt := "-"
				if t.LatestRTTMs != nil {
					rtt = fmtMs(*t.LatestRTTMs)
				}
				fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%d\t%s\t%s\t%s\n",
					t.ID, t.Operation, t.Mode, state, t.Changes, fmtAgo(t.LastChange), trackRC(t), rtt)
			}
			return tw.Flush()
		},
	}
}

// trackArgs accepts at most one track number, in the range the
// configuration accepts for tracks[].id (1..2147483647; Cisco's own track
// numbers stop at 1000).
func trackArgs(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) > 1:
		return usageError(cmd, fmt.Errorf("too many arguments: %s", strings.Join(args, " ")))
	case len(args) == 1:
		if n, err := strconv.Atoi(args[0]); err != nil || n < 1 || n > config.MaxOpID {
			return usageError(cmd, fmt.Errorf("invalid track number %q: must be between 1 and %d", args[0], config.MaxOpID))
		}
	}
	return nil
}

// trackRC is the latest return code the track judged, "-" before any.
func trackRC(t api.TrackJSON) string {
	if t.LatestRC == 0 {
		return "-"
	}
	return t.LatestRC.String()
}

// writeTrack prints one track like Cisco's show track:
//
//	Track 1
//	  IP SLA 13 reachability
//	  Reachability is Up
//	    2 changes, last change 00:00:47
//	  Delay up 5 secs, down 0 secs
//	  Latest operation return code: OK
//	  Latest RTT (millisecs) 0.123
func writeTrack(w io.Writer, t api.TrackJSON) error {
	what := "State"
	if t.Mode == "reachability" {
		what = "Reachability"
	}
	fmt.Fprintf(w, "Track %d\n", t.ID)
	fmt.Fprintf(w, "  IP SLA %d %s\n", t.Operation, t.Mode)
	fmt.Fprintf(w, "  %s is %s\n", what, capitalTrackState(t.State))
	if t.Pending != "" {
		fmt.Fprintf(w, "    %s pending (delay running)\n", capitalTrackState(t.Pending))
	}
	last := "never"
	if t.LastChange != nil {
		last = sinceTrackChange(*t.LastChange)
	}
	fmt.Fprintf(w, "    %d change%s, last change %s\n", t.Changes, pluralTrackChanges(t.Changes), last)
	up, _ := time.ParseDuration(t.DelayUp)
	down, _ := time.ParseDuration(t.DelayDown)
	if up > 0 || down > 0 {
		fmt.Fprintf(w, "  Delay up %d secs, down %d secs\n", int(up.Seconds()), int(down.Seconds()))
	}
	if t.LatestRC != 0 {
		fmt.Fprintf(w, "  Latest operation return code: %s\n", t.LatestRC.Display())
	}
	if t.LatestRTTMs != nil {
		fmt.Fprintf(w, "  Latest RTT (millisecs) %s\n", fmtMs(*t.LatestRTTMs))
	}
	return nil
}

func capitalTrackState(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func pluralTrackChanges(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sinceTrackChange is the time since t as hh:mm:ss (Cisco's "last change").
func sinceTrackChange(t time.Time) string {
	d := fmtNow().Sub(t).Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
