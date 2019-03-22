package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/suite"
)

/* Runs HTTP API tests of chat application between 2 skywire-node`s

Set envvar SKYWIRE_INTEGRATION_TESTS=1 to enable them
Set SKYWIRE_HOST to the first skywire-node's address
Set SKYWIRE_NODE to the second skywire-node's address
Set SKYWIRE_NODE_PK to static_public_key of SKYWIRE_NODE

E.g.
```bash
export SW_INTEGRATION_TESTS=1
export SW_NODE_A=127.0.0.1
export SW_NODE_A_PK=$(cat ./skywire.json|grep static_public_key |cut -d ':' -f2 |tr -d '"'','' ')
export SW_NODE_B=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' SKY01)
export SW_NODE_B_PK=$(cat ./node/skywire.json|grep static_public_key |cut -d ':' -f2 |tr -d '"'','' ')%
```

Preparation

*/

// Test suite for testing chat between 2 nodes
type TwoNodesSuite struct {
	suite.Suite
	Disabled bool
	Host     string
	HostPK   string
	Node     string
	NodePK   string
}

func (suite *TwoNodesSuite) SetupTest() {
	envEnabled := os.Getenv("SW_INTEGRATION_TESTS")
	suite.Disabled = (envEnabled != "1")
	suite.Host = os.Getenv("SW_NODE_A")
	suite.HostPK = os.Getenv("SW_NODE_A_PK")
	suite.Node = os.Getenv("SW_NODE_B")
	suite.NodePK = os.Getenv("SW_NODE_B_PK")

	suite.T().Logf(`
	SW_INTEGRATION_TESTS=%v 
	SW_NODE_A=%v SW_NODE_A_PK=%v 
	SW_NODE_B=%v SW_NODE_B_PK=%v`,
		envEnabled, suite.Host, suite.HostPK, suite.Node, suite.NodePK)
}

func TestTwoNodesSuite(t *testing.T) {
	suite.Run(t, new(TwoNodesSuite))
}

func (suite *TwoNodesSuite) Enabled() bool {
	if suite.Disabled {
		suite.T().Skip("Skywire tests are skipped")
		return false
	}
	return true
}

func sendmessage(nodeAddress, recipient, message string) (*http.Response, error) {
	data, _ := json.Marshal(map[string]string{"message": message, "recipient": recipient})
	return http.Post(nodeAddress, "application/json", bytes.NewReader(data))
}

func (suite *TwoNodesSuite) MessageToNode(message string) (*http.Response, error) {
	return sendmessage(suite.Host, suite.NodePK, message)
}

func (suite *TwoNodesSuite) MessageToHost(message string) (*http.Response, error) {
	return sendmessage(suite.Node, suite.HostPK, message)
}

func (suite *TwoNodesSuite) TestMessageToHost(message string) {
	t := suite.T()
	if suite.Enabled() {
		resp, err := suite.MessageToHost("Disabled")
		// require.Nil(t, err, "Got an error in MessageToHost")
		t.Logf("%v %v", resp, err)
	}
}

func (suite *TwoNodesSuite) TestMessageToNode(message string) {
	tx := suite.T()
	if suite.Enabled() {
		resp, err := suite.MessageToHost("B")
		// require.NoError(t, err, "Got an error in MessageToNode")
		t.Logf("%v %v", resp, err)
	}
}

func (suite *TwoNodesSuite) TestHelloMikeHelloJoe() {
	t := suite.T()
	if suite.Enabled() {
		resp, err := suite.MessageToNode("Hello Mike!")
		t.Logf("%v %v", resp, err)
		suite.MessageToHost("Hello Joe!")
		t.Logf("%v %v", resp, err)
		suite.MessageToNode("System is working!")
		t.Logf("%v %v", resp, err)
	}
}
