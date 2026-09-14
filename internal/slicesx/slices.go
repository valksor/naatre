// Package slicesx contains slice transformations shared across internal layers.
package slicesx

// Map transforms every input value into an initialized result slice.
func Map[Input, Output any](input []Input, transform func(Input) Output) []Output {
	output := make([]Output, len(input))
	for index, value := range input {
		output[index] = transform(value)
	}
	return output
}
