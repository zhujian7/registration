package spoke

import (
	"github.com/spf13/cobra"

	"github.com/openshift/library-go/pkg/controller/controllercmd"

	"open-cluster-management.io/registration/pkg/spoke"
	"open-cluster-management.io/registration/pkg/version"
)

func NewAgent() *cobra.Command {
	spokeAgent := spoke.NewSpokeAgent()
	cmd := controllercmd.
		NewControllerCommandConfig("registration-agent", version.Get(), spokeAgent.Run).
		NewCommand()
	cmd.Use = "agent"
	cmd.Short = "Start the Cluster Registration Agent"

	spokeAgent.AddFlags(cmd.Flags())
	return cmd
}
