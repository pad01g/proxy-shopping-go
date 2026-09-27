// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC20} from "../../src/interfaces/IERC20.sol";

/// 参考実装: operator と escrow の間の任意の規約（プロトコルの外）。
/// escrow が USDC を預け、不正な裁定があれば operator が没収して被害者の補償に回す。
/// 引き出しは申請から `withdrawDelay` 秒後の `withdrawWindow` 秒間だけ。その間も没収できる。
/// - 窓を過ぎた申請は無効（早くに申請しておき、不正の直後に引き出すことはできない）。
/// - 預け足すと申請は取り消される（申請後に預けた分も delay を待つ）。
/// - 没収されると申請は取り消される（通報の調査中に残りを引き出せない）。
contract PSBond {
    IERC20 public immutable token;
    address public immutable operator;
    uint64 public immutable withdrawDelay;
    uint64 public immutable withdrawWindow;

    mapping(address => uint256) public bondOf;
    /// 引き出しできるようになる時刻（0 = 申請なし）
    mapping(address => uint64) public withdrawableAt;

    event Deposited(address indexed escrow, uint256 amount, uint256 total);
    event Slashed(address indexed escrow, address indexed to, uint256 amount, uint256 remaining);
    event WithdrawRequested(address indexed escrow, uint64 withdrawableAt);
    event WithdrawCancelled(address indexed escrow);
    event Withdrawn(address indexed escrow, uint256 amount);

    error Unauthorized();
    error InsufficientBond();
    error NotRequested();
    error TooEarly();
    error TooLate();
    error TransferFailed();

    constructor(IERC20 token_, address operator_, uint64 withdrawDelay_, uint64 withdrawWindow_) {
        token = token_;
        operator = operator_;
        withdrawDelay = withdrawDelay_;
        withdrawWindow = withdrawWindow_;
    }

    /// 呼び出し元（escrow）自身の預け金に足す。先に token の approve が要る。引き出しの申請は取り消される。
    function deposit(uint256 amount) external {
        if (!token.transferFrom(msg.sender, address(this), amount)) revert TransferFailed();
        bondOf[msg.sender] += amount;
        _cancel(msg.sender);
        emit Deposited(msg.sender, amount, bondOf[msg.sender]);
    }

    /// 没収する。escrow の引き出しの申請は取り消される。
    function slash(address escrow, address to, uint256 amount) external {
        if (msg.sender != operator) revert Unauthorized();
        uint256 bond = bondOf[escrow];
        if (amount > bond) revert InsufficientBond();
        bondOf[escrow] = bond - amount;
        _cancel(escrow);
        if (!token.transfer(to, amount)) revert TransferFailed();
        emit Slashed(escrow, to, amount, bond - amount);
    }

    function requestWithdraw() external {
        uint64 at = uint64(block.timestamp) + withdrawDelay;
        withdrawableAt[msg.sender] = at;
        emit WithdrawRequested(msg.sender, at);
    }

    /// 残っている預け金を全額引き出す。申請から withdrawDelay 秒後の withdrawWindow 秒間だけ。
    function withdraw() external {
        uint64 at = withdrawableAt[msg.sender];
        if (at == 0) revert NotRequested();
        if (block.timestamp < at) revert TooEarly();
        if (block.timestamp > uint256(at) + withdrawWindow) revert TooLate();
        uint256 amount = bondOf[msg.sender];
        bondOf[msg.sender] = 0;
        withdrawableAt[msg.sender] = 0;
        if (!token.transfer(msg.sender, amount)) revert TransferFailed();
        emit Withdrawn(msg.sender, amount);
    }

    function _cancel(address escrow) internal {
        if (withdrawableAt[escrow] != 0) {
            withdrawableAt[escrow] = 0;
            emit WithdrawCancelled(escrow);
        }
    }
}
