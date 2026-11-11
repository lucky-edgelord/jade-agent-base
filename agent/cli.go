package agent

import (
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

// Command is `jab agent …`: create agents, issue and revoke tokens, list them.
func Command(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "agent", Short: "Manage the agents that run this server"}
	var scopes string
	var days int
	var desc string
	create := &cobra.Command{
		Use: "create <name>", Short: "Create an agent and print its token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc := []string{}
			for _, s := range strings.Split(scopes, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sc = append(sc, s)
				}
			}
			if scopes == "all" {
				sc = AllScopes
			}
			r, tok, err := CreateAgent(app, args[0], sc, desc, days)
			if err != nil {
				return err
			}
			fmt.Printf("agent %s (%s), scopes %s, token valid %d days:\n%s\n", r.GetString("name"), r.Id, strings.Join(sc, ","), max(days, 30*boolInt(days <= 0)), tok)
			return nil
		},
	}
	create.Flags().StringVar(&scopes, "scopes", "read", "comma-separated: "+strings.Join(AllScopes, ",")+" (or all)")
	create.Flags().IntVar(&days, "days", 30, "how long the token is valid")
	create.Flags().StringVar(&desc, "description", "", "what the agent is for")

	token := &cobra.Command{
		Use: "token <id or name>", Short: "Print a new token for an agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := find(app, args[0])
			if err != nil {
				return err
			}
			tok, err := Token(r, days)
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
	token.Flags().IntVar(&days, "days", 30, "how long the token is valid")
	revoke := &cobra.Command{
		Use: "revoke <id or name>", Short: "End every token an agent holds", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := find(app, args[0])
			if err != nil {
				return err
			}
			if err := Revoke(app, r); err != nil {
				return err
			}
			fmt.Println("revoked all tokens of", r.GetString("name"))
			return nil
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			rs, err := app.FindAllRecords(AgentsCollection)
			if err != nil {
				return err
			}
			for _, r := range rs {
				fmt.Printf("%s  %-24s %s\n", r.Id, r.GetString("name"), strings.Join(r.GetStringSlice("scopes"), ","))
			}
			return nil
		},
	}
	root.AddCommand(create, token, revoke, list)
	return root
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func find(app core.App, idOrName string) (*core.Record, error) {
	if r, err := app.FindRecordById(AgentsCollection, idOrName); err == nil {
		return r, nil
	}
	r, err := app.FindFirstRecordByData(AgentsCollection, "name", idOrName)
	if err != nil {
		return nil, fmt.Errorf("no agent %q", idOrName)
	}
	return r, nil
}
