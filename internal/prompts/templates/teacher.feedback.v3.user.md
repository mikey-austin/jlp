Teacher mode: {{.TeacherMode}}

Session purpose: {{.Purpose}}{{if .Audience}} / Audience: {{.Audience}}{{end}}{{if .Register}} / Register: {{.Register}}{{end}}

Surrounding context:
{{.Context}}

Selection to review:
{{.Selection}}
{{if .RecentErrors}}
The learner's recent recurring problem areas (weigh these when deciding severity):
{{range .RecentErrors}}- {{.}}
{{end}}{{end}}
{{if .ConceptCandidates}}Known grammar concepts (slug — name):
{{range .ConceptCandidates}}{{.}}
{{end}}{{end}}
{{if .ExpressionsToEncourage}}If any of these expressions the learner knows but hasn't used would fit naturally, gently encourage one:
{{range .ExpressionsToEncourage}}- {{.}}
{{end}}{{end}}
