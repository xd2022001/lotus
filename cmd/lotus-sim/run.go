package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
)

var runSimCommand = &cli.Command{
	Name:        "run",
	Description: "Run the simulation.",
	Flags: []cli.Flag{
		&cli.IntFlag{
			Name:  "epochs",
			Usage: "Advance the given number of epochs then stop.",
		},
	},
	Action: func(cctx *cli.Context) error {
		node, err := open(cctx)
		if err != nil {
			return err
		}
		defer node.Close()

		go profileOnSignal(cctx, syscall.SIGUSR2)

		sim, err := node.LoadSim(cctx.Context, cctx.String("simulation"))
		if err != nil {
			return err
		}
		fmt.Fprintln(cctx.App.Writer, "loading simulation")
		err = sim.Load(cctx.Context)
		if err != nil {
			return err
		}
		fmt.Fprintln(cctx.App.Writer, "running simulation")
		targetEpochs := cctx.Int("epochs")

		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGUSR1)
		defer signal.Stop(ch)

		for i := 0; targetEpochs == 0 || i < targetEpochs; i++ {
			ts, err := sim.Step(cctx.Context)
			if err != nil {
				return err
			}

			fmt.Fprintf(cctx.App.Writer, "advanced to %d %s\n", ts.Height(), ts.Key())

			// Print
			select {
			case <-ch:
				if err := printInfo(cctx.Context, sim, cctx.App.Writer); err != nil {
					fmt.Fprintf(cctx.App.ErrWriter, "ERROR: failed to print info: %s\n", err)
				}
			case <-cctx.Context.Done():
				return cctx.Err()
			default:
			}
		}
		fmt.Fprintln(cctx.App.Writer, "simulation done")
		return err
	},
}
