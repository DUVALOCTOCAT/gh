package action

import (
	"github.com/rsteube/carapace"
	"github.com/spf13/cobra"
)

type project struct {
	Name string
	Body string
}

type projectQuery struct {
	Data struct {
		Repository struct {
			Projects struct {
				Nodes []project
			}
		}
	}
}

func ActionProjects(cmd *cobra.Command) carapace.Action {
	return carapace.ActionCallback(func(args []string) carapace.Action {
		var queryResult projectQuery
		return GraphQlAction(cmd, `repository(owner: $owner, name: $repo){ projects(first: 100) { nodes { name, body } } }`, &queryResult, func() carapace.Action {
			projects := queryResult.Data.Repository.Projects.Nodes
			vals := make([]string, len(projects)*2)
			for index, p := range projects {
				vals[index*2] = p.Name
				vals[index*2+1] = p.Body
			}
			return carapace.ActionValuesDescribed(vals...)
		})
	})
}
