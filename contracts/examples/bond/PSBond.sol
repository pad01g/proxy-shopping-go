// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC20} from "../../src/interfaces/IERC20.sol";

/// 参考実装: operator と escrow の間の任意の規約（プロトコルの外）。
/// escrow が USDC を預け、不正な裁定があれば operator が没収して被害者の補償に回す。
/// 引き出しは申請から `withdrawDelay` 秒後。その間も没収できる。
contract PSBond {
    IERC20 public immutable token;
    address public immutable operator;
    uint64 public immutable withdrawDelay;

    mapping(address => uint256) public bondOf;
    /// 引き出しできるようになる時刻（0 = 申請なし）
    mapping(address => uint64) public withdrawableAt;

    event Deposited(address indexed escrow, uint256 amount, uint256 total);
    event Slashed(address indexed escrow, address indexed to, uint256 amount, uint256 remaining);
    event WithdrawRequested(address indexed escrow, uint64 withdrawableAt);
    event Withdrawn(address indexed escrow, uint256 amount);

    error Unauthorized();
    error InsufficientBond();
    error NotRequested();
    error TooEarly();
    error TransferFailed();

    constructor(IERC20 token_, address operator_, uint64 withdrawDelay_) {
        token = token_;
        operator = operator_;
        withdrawDelay = withdrawDelay_;
    }

    /// 呼び出し元（escrow）自身の預け金に足す。先に token の approve が要る。
    function deposit(uint256 amount) external {
        if (!token.transferFrom(msg.sender, address(this), amount)) revert TransferFailed();
        bondOf[msg.sender] += amount;
        emit Deposited(msg.sender, amount, bondOf[msg.sender]);
    }

    function slash(address escrow, address to, uint256 amount) external {
        if (msg.sender != operator) revert Unauthorized();
        uint256 bond = bondOf[escrow];
        if (amount > bond) revert InsufficientBond();
        bondOf[escrow] = bond - amount;
        if (!token.transfer(to, amount)) revert TransferFailed();
        emit Slashed(escrow, to, amount, bond - amount);
    }

    function requestWithdraw() external {
        uint64 at = uint64(block.timestamp) + withdrawDelay;
        withdrawableAt[msg.sender] = at;
        emit WithdrawRequested(msg.sender, at);
    }

    /// 残っている預け金を全額引き出す。
    function withdraw() external {
        uint64 at = withdrawableAt[msg.sender];
        if (at == 0) revert NotRequested();
        if (block.timestamp < at) revert TooEarly();
        uint256 amount = bondOf[msg.sender];
        bondOf[msg.sender] = 0;
        withdrawableAt[msg.sender] = 0;
        if (!token.transfer(msg.sender, amount)) revert TransferFailed();
        emit Withdrawn(msg.sender, amount);
    }
}
