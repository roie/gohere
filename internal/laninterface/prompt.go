package laninterface

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func PromptChooser(input io.Reader, output io.Writer) Chooser {
	return func(candidates []Candidate) (int, error) {
		if len(candidates) == 0 {
			return -1, ErrInvalidChoice
		}
		fmt.Fprintln(output, "Choose a network for LAN sharing:")
		fmt.Fprintln(output)
		for index, candidate := range candidates {
			fmt.Fprintf(output, "%d. %-12s %s\n", index+1, candidate.Name, candidate.Prefix.Addr())
		}
		fmt.Fprintln(output)
		fmt.Fprint(output, "Network [1]: ")
		reader := bufio.NewReader(input)
		answer, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return -1, err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return 0, nil
		}
		choice, err := strconv.Atoi(answer)
		if err != nil || choice < 1 || choice > len(candidates) {
			return -1, ErrInvalidChoice
		}
		return choice - 1, nil
	}
}
