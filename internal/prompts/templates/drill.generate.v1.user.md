Grammar concept: {{.ConceptSlug}} — {{.ConceptName}}
{{.ConceptDescription}}
{{if .ConceptExamples}}
Examples:
{{range .ConceptExamples}}- {{.}}
{{end}}{{end}}{{if .RecentErrors}}
The learner's recent recurring problem areas (target these if they fit this concept):
{{range .RecentErrors}}- {{.}}
{{end}}{{end}}
