#!/usr/bin/env python3
"""
Simple HTTP server to control k3d nodes
Usage: python3 vm_control_server.py [port]
Default port: 8080
"""

import subprocess
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs


class VMControlHandler(BaseHTTPRequestHandler):
    """HTTP request handler for VM control operations"""
    
    def do_GET(self):
        """Handle GET requests for /start and /stop endpoints"""
        parsed_path = urlparse(self.path)
        query_params = parse_qs(parsed_path.query)
        
        # Get VM name from query parameter
        vm_name = query_params.get('vm', [None])[0]
        
        if not vm_name:
            self.send_error_response(400, "Missing 'vm' parameter")
            return
        
        # Route to appropriate handler
        if parsed_path.path == '/start':
            self.handle_start(vm_name)
        elif parsed_path.path == '/stop':
            self.handle_stop(vm_name)
        else:
            self.send_error_response(404, f"Unknown endpoint: {parsed_path.path}")
    
    def handle_start(self, vm_name):
        """Start a k3d node"""
        try:
            result = subprocess.run(
                ['k3d', 'node', 'start', vm_name],
                capture_output=True,
                text=True,
                timeout=30
            )
            
            if result.returncode == 0:
                self.send_success_response(f"VM '{vm_name}' started successfully")
            else:
                self.send_error_response(500, f"Failed to start VM: {result.stderr}")
        
        except subprocess.TimeoutExpired:
            self.send_error_response(504, "Command timed out")
        except Exception as e:
            self.send_error_response(500, f"Error: {str(e)}")
    
    def handle_stop(self, vm_name):
        """Stop a k3d node"""
        try:
            result = subprocess.run(
                ['k3d', 'node', 'stop', vm_name],
                capture_output=True,
                text=True,
                timeout=30
            )
            
            if result.returncode == 0:
                self.send_success_response(f"VM '{vm_name}' shutdown signal sent")
            else:
                self.send_error_response(500, f"Failed to stop VM: {result.stderr}")
        
        except subprocess.TimeoutExpired:
            self.send_error_response(504, "Command timed out")
        except Exception as e:
            self.send_error_response(500, f"Error: {str(e)}")
    
    def send_success_response(self, message):
        """Send a successful JSON response"""
        self.send_response(200)
        self.send_header('Content-type', 'application/json')
        self.end_headers()
        response = f'{{"status": "success", "message": "{message}"}}'
        self.wfile.write(response.encode())
    
    def send_error_response(self, code, message):
        """Send an error JSON response"""
        self.send_response(code)
        self.send_header('Content-type', 'application/json')
        self.end_headers()
        response = f'{{"status": "error", "message": "{message}"}}'
        self.wfile.write(response.encode())
    
    def log_message(self, format, *args):
        """Override to customize logging"""
        sys.stdout.write(f"[{self.log_date_time_string()}] {format % args}\n")


def run_server(port=8080):
    """Start the HTTP server"""
    server_address = ('', port)
    httpd = HTTPServer(server_address, VMControlHandler)
    
    print(f"VM Control Server started on port {port}")
    print(f"Endpoints:")
    print(f"  - Start VM: http://localhost:{port}/start?vm=VM_NAME")
    print(f"  - Stop VM:  http://localhost:{port}/stop?vm=VM_NAME")
    print(f"\nPress Ctrl+C to stop the server\n")
    
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down server...")
        httpd.shutdown()


if __name__ == '__main__':
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    run_server(port)