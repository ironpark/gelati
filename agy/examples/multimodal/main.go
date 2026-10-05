// Command multimodal passes an image, a document and (optionally) audio to
// the agent, asks it to generate an image, and has a custom tool return an
// image the model can see (upstream examples/getting_started/multimodal.py).
//
// Without -image, a generated PNG is used.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"

	"github.com/ironpark/gelati/agy"
)

func main() {
	imagePath := flag.String("image", "", "image file to describe (default: a generated red circle)")
	audioPath := flag.String("audio", "", "audio file to transcribe (skipped when empty)")
	flag.Parse()
	ctx := context.Background()

	img, err := loadImage(*imagePath)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("  --- Multimodal input: image ---")
	run(ctx, agy.Options{}, agy.Text("What is in this image?"), img)

	fmt.Println("  --- Multimodal input: document ---")
	doc, err := agy.NewDocument([]byte("Project Falcon status report.\n\n"+
		"The secret number is 42. The team shipped the beta on time and is now focused on performance.\n"),
		"text/plain", "status report")
	if err != nil {
		log.Fatal(err)
	}
	run(ctx, agy.Options{}, agy.Text("Summarize this document"), doc)

	if *audioPath != "" {
		fmt.Println("  --- Multimodal input: audio ---")
		audio, err := agy.AudioFromFile(*audioPath, "")
		if err != nil {
			log.Fatal(err)
		}
		run(ctx, agy.Options{}, agy.Text("Transcribe or describe this audio clip"), audio)
	}

	fmt.Println("  --- Multimodal output: image generation ---")
	run(ctx, agy.Options{
		Capabilities: &agy.CapabilitiesConfig{EnabledTools: []agy.BuiltinTool{agy.BuiltinGenerateImage}},
	}, agy.Text("Generate an image of a futuristic city with a 16:9 aspect ratio, name it 'future_city'. "+
		"Please provide the file path to the generated image."))

	fmt.Println("  --- Multimodal tool output: a tool returns an image ---")
	// Media values anywhere in a tool result are sent to the model as
	// attachments, next to the rest of the result.
	loadExampleImage := agy.NewTool("load_example_image", "Loads the example image so you can see it.",
		func(context.Context, *agy.ToolContext, struct{}) ([]any, error) {
			return []any{"Here is the requested image.", img}, nil
		})
	run(ctx, agy.Options{Tools: []*agy.Tool{loadExampleImage}},
		agy.Text("Call load_example_image, then describe what is in the image."))
}

// loadImage reads path, or draws a red circle on white when path is empty.
func loadImage(path string) (*agy.Image, error) {
	if path != "" {
		return agy.ImageFromFile(path, "")
	}
	const size = 128
	m := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			dx, dy := x-size/2, y-size/2
			c := color.RGBA{255, 255, 255, 255}
			if dx*dx+dy*dy < (size/3)*(size/3) {
				c = color.RGBA{220, 20, 20, 255}
			}
			m.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		return nil, err
	}
	return agy.NewImage(buf.Bytes(), "image/png", "a generated test image")
}

// run starts a session with opts, sends the prompt parts and prints the
// answer.
func run(ctx context.Context, opts agy.Options, prompt ...agy.Content) {
	agent, err := agy.NewAgent(opts)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
	fmt.Println("  User:", prompt[0])
	stream, err := agent.Chat(ctx, prompt...)
	if err != nil {
		log.Fatal(err)
	}
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  Agent: %s\n\n", res.Text())
}
