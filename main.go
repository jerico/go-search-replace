package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"unsafe"

	"github.com/Automattic/go-search-replace/searchreplace"
)

const (
	badInputRe   = `\w:\d+:`
	inputRe      = `^[A-Za-z0-9_\-\.:/]+$`
	minInLength  = 4
	minOutLength = 2

	version = "0.0.11"
)

var (
	input = regexp.MustCompile(inputRe)
	bad   = regexp.MustCompile(badInputRe)
	// Regex to match mydumper size declarations: -- filename SIZE or -- metadata.header SIZE
	sizeDeclarationRegex = regexp.MustCompile(`^--\s+([^\s]+)\s+(\d+)$`)
)

func main() {
	versionFlag := flag.Bool("version", false, "Show version information")
	mydumperFlag := flag.Bool("mydumper", false, "Fix mydumper size declarations for streaming (use with mydumper --stream)")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("go-search-replace version %s\n", version)
		os.Exit(0)
		return
	}

	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: search-replace <from> <to>")
		os.Exit(1)
		return
	}

	var replacements []*searchreplace.Replacement
	args := os.Args[1:]
	argStart := 0

	// Skip flags
	for argStart < len(args) && len(args[argStart]) > 0 && args[argStart][0] == '-' {
		argStart++
	}

	if (len(args)-argStart)%2 > 0 {
		fmt.Fprintln(os.Stderr, "All replacements must have a <from> and <to> value")
		os.Exit(1)
		return
	}

	var from, to string
	for i := 0; i < (len(args)-argStart)/2; i++ {
		from = args[argStart+i*2]
		if !validInput(from, minInLength) {
			fmt.Fprintln(os.Stderr, "Invalid <from> URL, minimum length is 4")
			os.Exit(2)
			return
		}

		to = args[argStart+i*2+1]
		if !validInput(to, minOutLength) {
			fmt.Fprintln(os.Stderr, "Invalid <to>, minimum length is 2")
			os.Exit(3)
			return
		}

		replacements = append(replacements, &searchreplace.Replacement{
			From: []byte(from),
			To:   []byte(to),
		})
	}

	if *mydumperFlag {
		processWithSizeFix(replacements)
	} else {
		processLineByLine(replacements)
	}
}

func processWithSizeFix(replacements []*searchreplace.Replacement) {
	bufferSize := 16 * 1024 * 1024 // 16 MB
	r := bufio.NewReaderSize(os.Stdin, bufferSize)
	
	var pendingLines [][]byte
	var inSizeSection bool
	var sizeDeclarationLine []byte
	
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					processLineWithSizeFix(&line, replacements, &pendingLines, &inSizeSection, &sizeDeclarationLine)
				}
				if inSizeSection {
					outputSizeSection(sizeDeclarationLine, pendingLines)
				}
				break
			}
			fmt.Fprintln(os.Stderr, err.Error())
			break
		}
		
		processLineWithSizeFix(&line, replacements, &pendingLines, &inSizeSection, &sizeDeclarationLine)
	}
}

func processLineWithSizeFix(line *[]byte, replacements []*searchreplace.Replacement, pendingLines *[][]byte, inSizeSection *bool, sizeDeclarationLine *[]byte) {
	lineCopy := make([]byte, len(*line))
	copy(lineCopy, *line)
	if len(lineCopy) > 0 && lineCopy[len(lineCopy)-1] == '\n' {
		lineCopy = lineCopy[:len(lineCopy)-1]
	}
	
	// Check if this is a size declaration line (-- filename SIZE)
	if match := sizeDeclarationRegex.FindSubmatch(lineCopy); match != nil {
		if *inSizeSection {
			outputSizeSection(*sizeDeclarationLine, *pendingLines)
		}
		*sizeDeclarationLine = make([]byte, len(lineCopy))
		copy(*sizeDeclarationLine, lineCopy)
		*pendingLines = [][]byte{}
		*inSizeSection = true
		return
	}
	
	// If we're in a size section, accumulate content
	if *inSizeSection {
		fixed := searchreplace.FixLine(&lineCopy, replacements)
		*pendingLines = append(*pendingLines, *fixed)
	} else {
		// Not in a size section, just process and output normally
		fixed := searchreplace.FixLine(&lineCopy, replacements)
		fmt.Print(unsafeGetString(*fixed))
		fmt.Print("\n")
	}
}

func outputSizeSection(sizeDeclarationLine []byte, contentLines [][]byte) {
	match := sizeDeclarationRegex.FindSubmatch(sizeDeclarationLine)
	if match == nil {
		fmt.Print(unsafeGetString(sizeDeclarationLine))
		fmt.Print("\n")
		for _, line := range contentLines {
			fmt.Print(unsafeGetString(line))
			fmt.Print("\n")
		}
		return
	}
	
	filename := string(match[1])
	var contentSize int
	// Count bytes: each line's content + newline after each line
	for _, cl := range contentLines {
		contentSize += len(cl)
		contentSize += 1 // newline after each line
	}
	
	fmt.Printf("-- %s %d\n", filename, contentSize)
	for _, line := range contentLines {
		fmt.Print(unsafeGetString(line))
		fmt.Print("\n")
	}
}

func processLineByLine(replacements []*searchreplace.Replacement) {
	var wg sync.WaitGroup
	lines := make(chan chan []byte, 10)

	wg.Add(1)
	go func() {
		defer wg.Done()

		bufferSize := 16 * 1024 * 1024 // 16 MB
		r := bufio.NewReaderSize(os.Stdin, bufferSize)
		for {
			line, err := r.ReadBytes('\n')

			if err != nil {
				if err == io.EOF {
					if 0 == len(line) {
						break
					}
				} else {
					fmt.Fprintln(os.Stderr, err.Error())
					break
				}
			}

			wg.Add(1)
			ch := make(chan []byte)
			lines <- ch

			go func(line *[]byte) {
				defer wg.Done()
				line = searchreplace.FixLine(line, replacements)
				ch <- *line
			}(&line)
		}
	}()

	go func() {
		wg.Wait()
		close(lines)
	}()

	for line := range lines {
		fmt.Print(unsafeGetString(<-line))
	}
}

func validInput(in string, length int) bool {
	if len(in) < length {
		return false
	}

	if !input.MatchString(in) {
		return false
	}

	if bad.MatchString(in) {
		return false
	}

	return true
}

func unsafeGetString(bs []byte) string {
	return *(*string)(unsafe.Pointer(&bs))
}
