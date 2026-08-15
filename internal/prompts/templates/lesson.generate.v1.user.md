Prepare a lesson guide for this learner.

{{if .Priorities}}Current priorities (heuristic planner, highest first):
{{range .Priorities}}- {{.}}
{{end}}{{end}}
{{if .ActivationExpressions}}Vocabulary ripe for activation (known but dormant):
{{range .ActivationExpressions}}- {{.}}
{{end}}{{end}}
{{if .RecentCorrections}}Recent writing corrections:
{{range .RecentCorrections}}- {{.}}
{{end}}{{end}}
{{if .ObservationSummaries}}Learner model observations:
{{range .ObservationSummaries}}- {{.}}
{{end}}{{end}}
