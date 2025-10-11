multipass transfer homelabautoscaler-controller.tar k3s-master:/home/ubuntu/homelabautoscaler-controller.tar

# 4. Import into k3s containerd on master
multipass exec k3s-master -- sudo k3s ctr images import /home/ubuntu/homelabautoscaler-controller.tar

# 5. Copy to worker nodes and import
multipass transfer homelabautoscaler-controller.tar k3s-worker1:/home/ubuntu/homelabautoscaler-controller.tar
multipass exec k3s-worker1 -- sudo k3s ctr images import /home/ubuntu/homelabautoscaler-controller.tar

multipass transfer homelabautoscaler-controller.tar k3s-worker2:/home/ubuntu/homelabautoscaler-controller.tar
multipass exec k3s-worker2 -- sudo k3s ctr images import /home/ubuntu/homelabautoscaler-controller.tar

# 6. Verify the image is loaded
multipass exec k3s-master -- sudo k3s ctr images ls | grep homelabautoscaler-controller
