Session purpose: {{.Purpose}}{{if .Audience}} / Audience: {{.Audience}}{{end}}{{if .Register}} / Register: {{.Register}}{{end}}

Surrounding context:
{{.Context}}

Selection to review:
{{.Selection}}
{{if .RecentErrors}}
The learner's recent recurring problem areas (weigh these when deciding severity):
{{range .RecentErrors}}- {{.}}
{{end}}{{end}}
