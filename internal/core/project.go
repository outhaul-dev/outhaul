package core

import "time"

// DefaultProjectID is the project created by migration; apps that don't pick
// a project land here, so first-app creation never requires a project step.
const DefaultProjectID int64 = 1

// Project groups related apps under one named workspace (a product, client,
// or initiative), after Dokploy's project model. Apps reference a project;
// deployments and the deploy pipeline never see one.
type Project struct {
	ID          int64
	Name        string // unique, human/URL friendly
	Description string
	CreatedAt   time.Time
}
