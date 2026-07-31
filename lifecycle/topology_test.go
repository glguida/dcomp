package lifecycle

import "testing"

func TestDockerResourceNamesPreserveTupleBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "system and component",
			left:  containerName("a-b", "c"),
			right: containerName("a", "b-c"),
		},
		{
			name:  "consumer and input",
			left:  linkNetworkName("demo", "a-b", "c"),
			right: linkNetworkName("demo", "a", "b-c"),
		},
		{
			name:  "component and volume",
			left:  volumeName("demo", "a-b", "c"),
			right: volumeName("demo", "a", "b-c"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.left == test.right {
				t.Fatalf("distinct resource tuples both map to %q", test.left)
			}
		})
	}
}
