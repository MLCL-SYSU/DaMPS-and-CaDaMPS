# On the Deadline-aware and Cost-aware Multipath Packet Scheduling in Mobile Applications

The implementation of two schedulers: DaMPS (deadline-aware) and CaDaMPS (deadline-aware and cost-aware)

## At the beginning

Our implementation is based on the MPQUIC-go codebase. 

**Please read https://multipath-quic.org/2017/12/09/artifacts-available.html to figure out how to setup the code.**



## Usage

- Install Go 1.17.4

- Clone the code reposition

  > git clone https://github.com/MLCL-SYSU/DaMPS-and-CaDaMPS.git

- Compile the MPQUIC server `server.exe` and MPQUIC client `client.exe`

  > go install ./...

- You can run any network script with MPQUIC server `server.exe` and MPQUIC client `client.exe`



## Requirements

- Go 1.17.3

- Ubuntu LTS (18.04)

## Implementation

- The implementation of **DaMPS**  and **CaDaMPS** can be found in `scheduler_opt.go` 
- When the ``banditAvailable`` is true and ``costConstraintAvailable`` is false, the scheduler is  **DaMPS**
- When the ``banditAvailable`` is true and ``costConstraintAvailable`` is true, the scheduler is **CaDaMPS**

