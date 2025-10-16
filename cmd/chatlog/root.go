package chatlog

import (
	"runtime"

	"github.com/sjzar/chatlog/internal/chatlog"
	"github.com/sjzar/chatlog/pkg/util"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

func init() {
	// windows only
	cobra.MousetrapHelpText = ""

	rootCmd.PersistentFlags().BoolVar(&Debug, "debug", false, "debug")
	rootCmd.PersistentFlags().BoolVar(&Console, "console", false, "run with console interface")
	rootCmd.PersistentPreRun = initLog
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Err(err).Msg("command execution failed")
	}
}

var rootCmd = &cobra.Command{
	Use:     "chatlog",
	Short:   "chatlog",
	Long:    `chatlog`,
	Example: `chatlog`,
	Args:    cobra.MinimumNArgs(0),
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
	PreRun: prepareRoot,
	Run:    Root,
}

func prepareRoot(cmd *cobra.Command, args []string) {
	if Console || !Debug {
		initTuiLog(cmd, args)
	}
}

func Root(cmd *cobra.Command, args []string) {
	m := chatlog.New()
	mode := chatlog.RunModeHeadless
	autoOpen := true
	if Console {
		mode = chatlog.RunModeConsole
		autoOpen = false
	}
	m.SetRunOptions(chatlog.RunOptions{
		Mode:               mode,
		AutoOpenBrowser:    autoOpen,
		AutoOpenBrowserSet: true,
	})

	if runtime.GOOS == "windows" && !Console && !Debug {
		util.HideConsoleWindow()
	}

	if err := m.Run(""); err != nil {
		log.Err(err).Msg("failed to run chatlog instance")
	}
}
