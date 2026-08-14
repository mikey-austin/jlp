package learner

type IdentityID string

type Identity struct {
	ID          IdentityID
	DisplayName string
	Attributes  map[string]string
}
