package minecraft

import (
	"fmt"

	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
)

const sampleConfig = `
  ## Address of the Minecraft server.
  # server = "localhost"

  ## Server RCON Port.
  # port = "25575"

  ## Server RCON Password.
  password = ""

  ## Uncomment to remove deprecated metric components.
  # tagdrop = ["server"]
`

// Client is a client for the Minecraft server.
type Client interface {
	// Connect establishes a connection to the server.
	Connect() error

	// Players returns the players on the scoreboard.
	Players() ([]string, error)

	// Scores return the objective scores for a player.
	Scores(player string) ([]Score, error)
}

// Minecraft is the plugin type.
type Minecraft struct {
	Server   string `toml:"server"`
	Port     string `toml:"port"`
	Password string `toml:"password"`

	client Client
}

func (s *Minecraft) Description() string {
	return "Collects scores from a Minecraft server's scoreboard using the RCON protocol"
}

func (s *Minecraft) SampleConfig() string {
	return sampleConfig
}

func (s *Minecraft) Gather(acc cua.Accumulator) error {
	if s.client == nil {
		connector := newConnector(s.Server, s.Port, s.Password)
		client := newClient(connector)
		s.client = client
	}

	players, err := s.client.Players()
	if err != nil {
		return fmt.Errorf("players: %w", err)
	}

	for _, player := range players {
		scores, err := s.client.Scores(player)
		if err != nil {
			return fmt.Errorf("scores: %w", err)
		}

		tags := map[string]string{
			"player": player,
			"server": s.Server + ":" + s.Port,
			"source": s.Server,
			"port":   s.Port,
		}

		var fields = make(map[string]interface{}, len(scores))
		for _, score := range scores {
			fields[score.Name] = score.Value
		}

		acc.AddFields("minecraft", fields, tags)
	}

	return nil
}

func init() {
	inputs.Add("minecraft", func() cua.Input {
		return &Minecraft{
			Server: "localhost",
			Port:   "25575",
		}
	})
}
