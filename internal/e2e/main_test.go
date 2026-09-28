package e2e_test

import (
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode) // no gin debug banner in the test output
	os.Exit(m.Run())
}
