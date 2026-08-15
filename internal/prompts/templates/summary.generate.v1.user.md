Write this learner's weekly progress digest email.

{{if .StatsSummary}}This week's activity:
{{range .StatsSummary}}- {{.}}
{{end}}{{end}}
{{if .Priorities}}Current priorities (heuristic planner, highest first):
{{range .Priorities}}- {{.}}
{{end}}{{end}}
{{if .NewExpressions}}Vocabulary recently active:
{{range .NewExpressions}}- {{.}}
{{end}}{{end}}
