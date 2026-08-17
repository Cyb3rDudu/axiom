package main
import (
	"fmt"
	ort "github.com/yalue/onnxruntime_go"
)
func main() {
	ort.SetSharedLibraryPath("/opt/onnxruntime/lib/libonnxruntime.so.1.29.0")
	if err := ort.InitializeEnvironment(); err != nil { fmt.Println("env err:", err); return }
	// reinstall signal handlers per the CUDA EP caveat
	defer ort.DestroyEnvironment()
	opts, err := ort.NewSessionOptions()
	if err != nil { fmt.Println("opts err:", err); return }
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil { fmt.Println("cuda opts err:", err); return }
	if err := cuda.Update(map[string]string{"device_id": "0"}); err != nil {
		fmt.Println("cuda update err:", err); return
	}
	defer cuda.Destroy()
	if err := opts.AppendExecutionProviderCUDA(cuda); err != nil {
		fmt.Println("CUDA EP append err:", err); return
	}
	defer opts.Destroy()
	fmt.Println("CUDA EP appended OK")
}
