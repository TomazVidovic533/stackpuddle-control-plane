package process_test

import (
	"testing"

	"github.com/TomazVidovic533/stackpuddle/model"
)

func parse(t *testing.T) (model.ProjectName, model.EnvironmentName) {
	t.Helper()
	project, err := model.ParseProjectName("shop")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := model.ParseEnvironmentName("dev")
	if err != nil {
		t.Fatal(err)
	}
	return project, environment
}
